package promote

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The two files of step-7b.md's grammar block; the headers are added.
const infraFile = `
version: 1
stack: infra
ladder:
  - staging
  - production
head: main
base:
  tiles:
    pg-db:
      kind: operated
      engine: postgres
      env_pairs:
        dev: staging
        staging: staging
        production: production
      allow:
        - testorg:shop:*
        - testorg:blog:*:api
`

const sliceShopFile = `
version: 1
stack: shop
ladder:
  - dev
  - prd
head: main
base:
  tiles:
    api-db:
      kind: slice
      provision_from: infra:${{ env.name }}:pg-db
      default_access: write
    api:
      image: ghcr.io/acme/api:1.4
      env:
        DATABASE_URL: ${{ tile.api-db.url }}
    reporter:
      kind: cron
      schedule: "0 3 * * *"
      slice_access:
        - from: api-db
          access: read
      env:
        DATABASE_URL: ${{ tile.api-db.url }}
`

// tiles is a stack file whose base holds body (tile entries, indented four).
func tiles(body string) string {
	return "version: 1\nstack: s\nbase:\n  tiles:\n" + body
}

func TestGrammarSlices(t *testing.T) {
	r, err := Load([]byte(infraFile), nil, "testorg")
	must(t, err)
	pg := r.Envs["staging"].Tiles["pg-db"]
	if pg.Type != tile.Operated || len(pg.Allow) != 2 || pg.EnvPairs["dev"] != "staging" {
		t.Errorf("pg-db = %+v", pg)
	}
	r, err = Load([]byte(sliceShopFile), nil, "testorg")
	must(t, err)
	dev := r.Envs["dev"]
	if s := dev.Tiles["api-db"]; s.Type != tile.Slice || s.ProvisionFrom != "infra:${{ env.name }}:pg-db" || s.DefaultAccess != "write" {
		t.Errorf("api-db = %+v", s)
	}
	if a := dev.Tiles["reporter"].SliceAccess; len(a) != 1 || a[0].From != "api-db" || a[0].Access != "read" {
		t.Errorf("reporter slice_access = %+v", a)
	}

	for name, c := range map[string]struct {
		file string
		want string
	}{
		"slices gone": {
			tiles("    api:\n      image: x\n      slices:\n        - db\n"),
			"tile api: slices: is gone; declare a slice tile, see DECIDE 194",
		},
		"shared gone": {
			"version: 1\nstack: s\nshared:\n  db:\n    engine: postgres\n",
			"shared: is gone; put the instance in its own stack and allow it, see DECIDE 193 and 194",
		},
		"allow on image": {
			tiles("    api:\n      image: x\n      allow:\n        - testorg:*\n"),
			"tile api: allow: only an operated tile takes it",
		},
		"env_pairs on service": {
			tiles("    api:\n      kind: service\n      env_pairs:\n        dev: staging\n"),
			"tile api: env_pairs: only an operated tile takes it",
		},
		"allow other org": {
			tiles("    db:\n      engine: postgres\n      allow:\n        - acme:*\n"),
			"tile db: allow: Start with this org, testorg.",
		},
		"env_pairs not slugs": {
			tiles("    db:\n      engine: postgres\n      env_pairs:\n        Dev: staging\n"),
			"tile db: env_pairs Dev: staging: both are environment names",
		},
		"provision_from on image": {
			tiles("    api:\n      image: x\n      provision_from: a:b:c\n"),
			"tile api: provision_from: only a slice tile takes it",
		},
		"slice needs target": {
			tiles("    s:\n      kind: slice\n"),
			"tile s: a slice tile needs provision_from: <stack>:<env>:<tile>",
		},
		"slice target shape": {
			tiles("    s:\n      kind: slice\n      provision_from: infra:pg-db\n"),
			"tile s: provision_from: infra:pg-db: three segments, <stack>:<env>:<tile>",
		},
		"on_remove on image": {
			tiles("    api:\n      image: x\n      on_remove: drop\n"),
			"tile api: on_remove: only a slice tile takes it",
		},
		"slice on_remove word": {
			tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      on_remove: detach\n"),
			`tile s: on_remove "detach" must be keep or drop`,
		},
		"bucket keys on image": {
			tiles("    api:\n      image: x\n      quota_gb: 5\n"),
			"tile api: version_days, quota_gb and cors_origins: only a slice tile takes them",
		},
		"slice version_days range": {
			tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      version_days: -2\n"),
			"version_days is 0 (versioning off) to 3650",
		},
		"slice quota overflow": {
			tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      quota_gb: 99999999999\n"),
			"quota_gb is 0 (no cap) to 1048576",
		},
		"slice cors origin": {
			tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      cors_origins:\n        - app.example.com\n"),
			`"app.example.com" is not an origin`,
		},
		"slice access word": {
			tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      default_access: admin\n"),
			`tile s: default_access "admin" must be read or write`,
		},
		"slice image": {
			tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      image: x\n"),
			"tile s: a slice tile takes no image",
		},
		"slice build": {
			tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      build:\n        context: .\n"),
			"tile s: a slice tile takes no build",
		},
		"slice port": {
			tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      port: 80\n"),
			"tile s: a slice tile takes no port",
		},
		"slice env": {
			tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      env:\n        A: b\n"),
			"tile s: a slice tile takes no env",
		},
		"slice domains": {
			tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      domains:\n        - auto: true\n"),
			"tile s: a slice tile takes no domains",
		},
		"slice volumes": {
			tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      volumes:\n        - data:/x\n"),
			"tile s: a slice tile takes no volumes",
		},
		"slice command": {
			tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      command: run\n"),
			"tile s: a slice tile takes no command",
		},
		"slice schedule": {
			tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      schedule: \"0 3 * * *\"\n"),
			"tile s: a slice tile takes no schedule",
		},
		"slice_access on operated": {
			tiles("    db:\n      engine: postgres\n      slice_access:\n        - from: s\n          access: read\n"),
			"tile db: slice_access: operated tiles bind to no slice",
		},
		"slice_access word": {
			tiles("    api:\n      image: x\n      slice_access:\n        - from: s\n          access: admin\n"),
			`tile api: slice_access s: access "admin" must be read or write`,
		},
		"slice_access not a slice": {
			tiles("    api:\n      image: x\n      slice_access:\n        - from: web\n          access: read\n    web:\n      image: x\n"),
			`tile api: slice_access from "web": no slice tile of that name in this environment`,
		},
	} {
		_, err := Load([]byte(c.file), nil, "testorg")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

// sliceWorld is setup's shop in org testorg, plus stack infra with envs
// staging and production, each with operated tile pg-db carrying the
// grammar's allow list and env pairs, and a running replica to exec in. The
// fake's psql answers "nothing exists yet" to every check.
type sliceWorld struct {
	*world
	infra store.Stack
	envs  map[string]store.Environment // infra's, by slug
	pg    map[string]store.Tile        // infra's pg-db, by env slug
}

func newSliceWorld(t *testing.T) *sliceWorld {
	w := &sliceWorld{
		world: setup(t),
		envs:  map[string]store.Environment{},
		pg:    map[string]store.Tile{},
	}
	o, err := w.s.Orgs.Get(ctx, w.st.OrgID)
	must(t, err)
	o.Slug = "testorg"
	must(t, w.s.Orgs.Update(ctx, o))
	now := time.Now()
	w.infra = store.Stack{
		ID:           uuid.NewString(),
		OrgID:        w.st.OrgID,
		Name:         "infra",
		Slug:         "infra",
		Settings:     "{}",
		ConfigRepo:   "https://github.com/acme/infra",
		ConfigBranch: "main",
		CreatedAt:    now,
	}
	must(t, w.s.Stacks.Create(ctx, w.infra))
	for i, name := range []string{"staging", "production"} {
		e := store.Environment{
			ID:         uuid.NewString(),
			StackID:    w.infra.ID,
			Name:       name,
			Slug:       name,
			Type:       environment.Static,
			Settings:   "{}",
			Network:    "infra-" + name,
			Position:   i,
			FromKind:   environment.FromPromote,
			FromBranch: "",
			CreatedAt:  now,
		}
		if i == 0 {
			e.FromKind, e.FromBranch = environment.FromBranch, "main"
		}
		must(t, w.s.Environments.Create(ctx, e))
		w.envs[name] = e
		w.pg[name] = w.operatedTile(t, e, []string{
			"testorg:shop:*",
			"testorg:blog:*:api",
		}, map[string]string{
			"dev":        "staging",
			"staging":    "staging",
			"production": "production",
		})
		w.fake.Containers = append(w.fake.Containers, docker.Container{
			ID:    "pg-" + name,
			State: "running",
			Labels: map[string]string{
				tile.LabelTile: w.pg[name].ID,
				tile.LabelRole: "replica",
			},
		})
	}
	w.fake.ExecOut = "0,0"
	return w
}

// operatedTile makes pg-db in e with its instance row.
func (w *sliceWorld) operatedTile(t *testing.T, e store.Environment, allow []string, pairs map[string]string) store.Tile {
	d := w.f.D
	pg, err := d.Tiles.Create(ctx, store.Tile{
		StackID:       e.StackID,
		EnvironmentID: e.ID,
		Name:          "pg-db",
		Slug:          "pg-db",
		Kind:          tile.Operated,
	})
	must(t, err)
	m, err := d.Operated.Create(ctx, pg.ID, "postgres", "stackr", "")
	must(t, err)
	m, err = d.Operated.SetAllow(ctx, m, "testorg", allow)
	must(t, err)
	_, err = d.Operated.SetEnvPairs(ctx, m, pairs)
	must(t, err)
	return pg
}

// instance edits the instance behind infra's pg-db in env.
func (w *sliceWorld) instance(t *testing.T, env string, edit func(*store.OperatedInstance)) {
	m, err := w.f.D.Operated.GetByTile(ctx, w.pg[env].ID)
	must(t, err)
	edit(&m)
	must(t, w.s.OperatedInstances.Update(ctx, m))
}

// shopRelease is a shop release of file at commit, with reporter's build
// pinned (it builds from git).
func (w *sliceWorld) shopRelease(t *testing.T, commit, file string) store.Release {
	w.files[commit] = file
	im, err := w.f.D.Images.Built(ctx, "shop/reporter:"+commit, "sha256:"+commit)
	must(t, err)
	return w.release(t, commit, release.Pin{Slug: "reporter", ImageID: &im.ID})
}

func TestSliceBlockers(t *testing.T) {
	for _, c := range []struct {
		name string
		file string // "" = sliceShopFile
		prep func(t *testing.T, w *sliceWorld)
		want string
	}{
		{
			name: "unknown stack",
			prep: func(t *testing.T, w *sliceWorld) {
				must(t, w.s.Stacks.Delete(ctx, w.infra.ID))
			},
			want: "slice api-db: stack infra not found",
		},
		{
			name: "no env pair",
			prep: func(t *testing.T, w *sliceWorld) {
				for _, env := range []string{"staging", "production"} {
					w.instance(t, env, func(m *store.OperatedInstance) {
						m.EnvPairs = store.StringMap{"staging": "staging"}
					})
				}
			},
			want: "slice api-db: infra's pg-db has no env pair for dev",
		},
		{
			name: "paired env missing",
			prep: func(t *testing.T, w *sliceWorld) {
				w.instance(t, "staging", func(m *store.OperatedInstance) {
					m.EnvPairs = store.StringMap{"dev": "qa"}
				})
			},
			want: "slice api-db: infra has no environment qa",
		},
		{
			name: "no env pairs, name as written",
			prep: func(t *testing.T, w *sliceWorld) {
				for _, env := range []string{"staging", "production"} {
					w.instance(t, env, func(m *store.OperatedInstance) {
						m.EnvPairs = nil
					})
				}
			},
			want: "slice api-db: infra has no environment dev",
		},
		{
			name: "s3 slice reads a postgres output",
			prep: func(t *testing.T, w *sliceWorld) {
				w.instance(t, "staging", func(m *store.OperatedInstance) { m.Engine = "s3" })
			},
			want: `environment dev tile api: refs ${{ tile.api-db.url }}: a s3 slice has no output "url"; valid: access_key, bucket, endpoint, public_url, region, secret_key`,
		},
		{
			name: "s3 slice url pooled",
			file: strings.Replace(sliceShopFile, "      default_access: write\n", "      default_access: write\n      url: pooled\n", 1),
			prep: func(t *testing.T, w *sliceWorld) {
				w.instance(t, "staging", func(m *store.OperatedInstance) { m.Engine = "s3" })
			},
			want: "slice api-db: url pooled applies to postgres slices only",
		},
		{
			name: "postgres slice reads an s3 output",
			file: strings.ReplaceAll(sliceShopFile, "tile.api-db.url", "tile.api-db.endpoint"),
			want: `environment dev tile api: refs ${{ tile.api-db.endpoint }}: a postgres slice has no output "endpoint"; valid: database, host, host_direct, host_pooled, host_read, host_read_direct, host_read_pooled, password, port, port_direct, port_pooled, port_read, port_read_direct, port_read_pooled, url, url_direct, url_pooled, url_read, url_read_direct, url_read_pooled, user`,
		},
		{
			name: "no tile",
			prep: func(t *testing.T, w *sliceWorld) {
				must(t, w.f.D.Tiles.Delete(ctx, w.pg["staging"].ID))
			},
			want: "slice api-db: infra/staging has no tile called pg-db to cut it from",
		},
		{
			name: "not operated",
			prep: func(t *testing.T, w *sliceWorld) {
				must(t, w.f.D.Tiles.Delete(ctx, w.pg["staging"].ID))
				_, err := w.f.D.Tiles.Create(ctx, store.Tile{
					StackID:       w.infra.ID,
					EnvironmentID: w.envs["staging"].ID,
					Name:          "pg-db",
					Slug:          "pg-db",
					Kind:          tile.Image,
					ImageRef:      "postgres:16",
				})
				must(t, err)
			},
			want: "slice api-db: infra/staging/pg-db is not an operated tile",
		},
		{
			name: "not allowed",
			prep: func(t *testing.T, w *sliceWorld) {
				w.instance(t, "staging", func(m *store.OperatedInstance) {
					m.Allow = store.StringList{"testorg:blog:*"}
				})
			},
			want: "slice api-db: infra/staging/pg-db does not allow testorg:shop:dev:api-db; add it to the Allow list of pg-db",
		},
		{
			name: "param unset",
			file: strings.Replace(sliceShopFile, "infra:${{ env.name }}", "infra:${{ params.db.env }}", 1),
			want: "slice api-db: provision_from needs params.db.env set first",
		},
		{
			name: "ref not allowed",
			file: strings.Replace(sliceShopFile, "infra:${{ env.name }}", "${{ tile.api.host }}:dev", 1),
			want: "slice api-db: provision_from: ${{ tile.api.host }}: a tile ref is not allowed in provision_from",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newSliceWorld(t)
			if c.prep != nil {
				c.prep(t, w)
			}
			file := sliceShopFile
			if c.file != "" {
				file = c.file
			}
			p, err := w.f.Plan(ctx, w.dev.ID, w.shopRelease(t, "c1", file).ID, io.Discard)
			must(t, err)
			if !slices.Contains(p.Blockers, c.want) {
				t.Errorf("blockers = %q, want %q", p.Blockers, c.want)
			}
		})
	}
}

// The grammar's two stacks: shop's dev plan reaches infra/staging/pg-db
// through env_pairs; the file lands in tile rows; a moved default access is
// a row and redeploys every consumer; a PR env maps as its base env.
func TestSliceHappyPath(t *testing.T) {
	w := newSliceWorld(t)
	r := w.shopRelease(t, "c1", sliceShopFile)
	p, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	if p.Blocked() {
		t.Fatalf("blockers = %q", p.Blockers)
	}
	got := kinds(p)
	for _, want := range []string{
		"create:api-db",
		"slice:api-dbinfra/staging/pg-db (write)",
		"create:reporter",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan = %s, want %s", got, want)
		}
	}

	_, err = w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
	must(t, err)
	sl, err := w.f.D.Tiles.GetBySlug(ctx, w.dev.ID, "api-db")
	must(t, err)
	if sl.Kind != tile.Slice || deref(sl.ProvisionFrom) != "infra:${{ env.name }}:pg-db" || deref(sl.DefaultAccess) != "write" {
		t.Errorf("slice row = %+v", sl)
	}
	rep, err := w.f.D.Tiles.GetBySlug(ctx, w.dev.ID, "reporter")
	must(t, err)
	want := store.SliceAccessList{
		{
			From:   "api-db",
			Access: "read",
		},
	}
	if !slices.Equal(rep.SliceAccess, want) {
		t.Errorf("reporter slice_access = %+v", rep.SliceAccess)
	}

	// default access moves: a row, and api and reporter redeploy.
	moved := strings.Replace(sliceShopFile, "default_access: write", "default_access: read", 1)
	p, wk, err := w.f.plan(ctx, w.dev.ID, w.shopRelease(t, "c2", moved).ID, io.Discard)
	must(t, err)
	if got := kinds(p); !strings.Contains(got, "slice:api-dbinfra/staging/pg-db (read)infra/staging/pg-db (write)") ||
		strings.Contains(got, "update:api-db") {
		t.Errorf("plan = %s, want the slice's access move as its one row", got)
	}
	if !wk.redeploy["api"] || !wk.redeploy["reporter"] {
		t.Errorf("redeploy = %v, want api and reporter", wk.redeploy)
	}

	// A PR env of dev has no env pair of its own: it maps as dev, and the
	// allow list sees its own slug.
	base := w.dev.ID
	pr := store.Environment{
		ID:         uuid.NewString(),
		StackID:    w.st.ID,
		Name:       "pr-12",
		Slug:       "pr-12",
		Type:       environment.Ephemeral,
		BaseEnvID:  &base,
		Settings:   "{}",
		Network:    "pr",
		Position:   2,
		FromKind:   environment.FromBranch,
		FromBranch: "feature",
		CreatedAt:  time.Now(),
	}
	must(t, w.s.Environments.Create(ctx, pr))
	p, err = w.f.Plan(ctx, pr.ID, w.shopRelease(t, "c3", sliceShopFile).ID, io.Discard)
	must(t, err)
	if p.Blocked() || !strings.Contains(kinds(p), "slice:api-dbinfra/staging/pg-db (write)") {
		t.Errorf("pr plan = %s, blockers %q", kinds(p), p.Blockers)
	}
	w.instance(t, "staging", func(m *store.OperatedInstance) {
		m.Allow = store.StringList{"testorg:shop:dev:*"}
	})
	p, err = w.f.Plan(ctx, pr.ID, w.shopRelease(t, "c4", sliceShopFile).ID, io.Discard)
	must(t, err)
	if want := "slice api-db: infra/staging/pg-db does not allow testorg:shop:pr-12:api-db; add it to the Allow list of pg-db"; !slices.Contains(p.Blockers, want) {
		t.Errorf("pr blockers = %q, want %q", p.Blockers, want)
	}
}

// sliceFile is sliceShopFile with reporter gone, or api-db gone too (and
// api's ref to it with it).
func sliceFile(t *testing.T, noReporter, noSlice bool) string {
	t.Helper()
	f := sliceShopFile
	if noReporter {
		before, _, ok := strings.Cut(f, "    reporter:\n")
		if !ok {
			t.Fatal("sliceFile: no reporter")
		}
		f = before
	}
	if noSlice {
		for _, cut := range []string{
			"    api-db:\n      kind: slice\n      provision_from: infra:${{ env.name }}:pg-db\n      default_access: write\n",
			"      env:\n        DATABASE_URL: ${{ tile.api-db.url }}\n",
		} {
			if !strings.Contains(f, cut) {
				t.Fatalf("sliceFile: no %q", cut)
			}
			f = strings.Replace(f, cut, "", 1)
		}
	}
	return f
}

// execs is every Exec from call n on, one per line.
func execs(w *sliceWorld, n int) string {
	var b strings.Builder
	for _, c := range w.fake.Calls()[n:] {
		if c.Method == "Exec" {
			b.WriteString(strings.Join(c.Args, " ") + "\n")
		}
	}
	return b.String()
}

func onNet(ns []docker.NetAttach, name string) bool {
	return slices.ContainsFunc(ns, func(n docker.NetAttach) bool {
		return n.Name == name
	})
}

// The grammar's two stacks deployed: api-db lands on infra/staging/pg-db
// as shop_dev_api_db (DECIDE 197), api gets a write user and reporter a
// read user, both on the instance's network. Removing reporter drops its
// user; the file then says on_remove: drop (DECIDE 199), and removing
// api-db drops the database and its bindings.
func TestSliceDeploy(t *testing.T) {
	w := newSliceWorld(t)
	d := w.f.D
	_, err := w.f.Apply(ctx, w.dev.ID, w.shopRelease(t, "c1", sliceShopFile).ID, io.Discard, nil)
	must(t, err)
	sl, err := d.Tiles.GetBySlug(ctx, w.dev.ID, "api-db")
	must(t, err)
	pr, ok, err := d.Operated.ProvisionOf(ctx, sl.ID)
	must(t, err)
	m, err := d.Operated.GetByTile(ctx, w.pg["staging"].ID)
	must(t, err)
	if !ok || pr.InstanceID != m.ID || pr.DBName != "shop_dev_api_db" {
		t.Fatalf("provision = %+v %v, want shop_dev_api_db on staging's pg-db", pr, ok)
	}
	bs, err := d.Operated.Bindings(ctx, pr.ID)
	must(t, err)
	got := map[string]string{}
	for _, b := range bs {
		got[b.DBUser] = b.Access
	}
	if len(got) != 2 || got["shop_dev_api_db_api"] != "write" || got["shop_dev_api_db_reporter"] != "read" {
		t.Fatalf("bindings = %v, want api write and reporter read", got)
	}

	// Both consumers sit on the instance's network and dial its alias there.
	net := operated.Network(m.ID)
	host := "pg-db-" + m.ID[:8]
	i := slices.IndexFunc(w.fake.Specs, func(s docker.ContainerSpec) bool {
		return strings.HasPrefix(s.Name, "stackr-api-")
	})
	if i < 0 || !onNet(w.fake.Specs[i].Networks, net) {
		t.Fatalf("api spec not on %s: %+v", net, w.fake.Specs)
	}
	if !slices.ContainsFunc(w.fake.Specs[i].Env, func(e string) bool {
		return strings.HasPrefix(e, "DATABASE_URL=postgres://shop_dev_api_db_api:") && strings.Contains(e, "@"+host+":5432/shop_dev_api_db")
	}) {
		t.Errorf("api env = %v", w.fake.Specs[i].Env)
	}
	rep, err := d.Tiles.GetBySlug(ctx, w.dev.ID, "reporter")
	must(t, err)
	dev, err := d.Envs.Get(ctx, w.dev.ID) // the release the apply set
	must(t, err)
	ref, _, err := d.Current(ctx, rep, dev)
	must(t, err)
	spec, err := d.Spec(ctx, rep, ref, io.Discard)
	must(t, err)
	if !onNet(spec.Networks, net) || !slices.ContainsFunc(spec.Env, func(e string) bool {
		return strings.HasPrefix(e, "DATABASE_URL=postgres://shop_dev_api_db_reporter:")
	}) {
		t.Errorf("reporter spec = %+v", spec)
	}

	// reporter goes: its user goes. api-db's file entry turns to drop.
	n := len(w.fake.Calls())
	c2 := strings.Replace(
		sliceFile(t, true, false),
		"      default_access: write\n",
		"      default_access: write\n      on_remove: drop\n",
		1,
	)
	r := w.shopRelease(t, "c2", c2)
	p, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	if !strings.Contains(kinds(p), "update:api-dbon_removedropkeep") {
		t.Errorf("c2 plan = %s, want api-db's on_remove row", kinds(p))
	}
	_, err = w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
	must(t, err)
	if ex := execs(w, n); !strings.Contains(ex, `DROP ROLE IF EXISTS "shop_dev_api_db_reporter"`) {
		t.Errorf("reporter removal execs:\n%s", ex)
	}
	if bs, _ := d.Operated.Bindings(ctx, pr.ID); len(bs) != 1 || bs[0].DBUser != "shop_dev_api_db_api" {
		t.Errorf("bindings after reporter = %+v", bs)
	}

	// api-db goes with on_remove drop: api's user, then the database.
	n = len(w.fake.Calls())
	_, err = w.f.Apply(ctx, w.dev.ID, w.shopRelease(t, "c3", sliceFile(t, true, true)).ID, io.Discard, nil)
	must(t, err)
	ex := execs(w, n)
	for _, want := range []string{
		`DROP ROLE IF EXISTS "shop_dev_api_db_api"`,
		`DROP DATABASE IF EXISTS "shop_dev_api_db" WITH (FORCE)`,
	} {
		if !strings.Contains(ex, want) {
			t.Errorf("api-db removal: no %q in\n%s", want, ex)
		}
	}
	if _, err := d.Operated.GetProvision(ctx, pr.ID); err == nil {
		t.Error("the dropped slice kept its provision")
	}
	if bs, _ := d.Operated.Bindings(ctx, pr.ID); len(bs) != 0 {
		t.Errorf("bindings after drop = %+v", bs)
	}
}

// A bucket slice's protection keys are rows in the plan, land on the slice
// tile, and a moved one redeploys the slice (which re-syncs its bucket).
func TestSliceBucketKeys(t *testing.T) {
	w := newSliceWorld(t)
	d := w.f.D
	_, err := w.f.Apply(ctx, w.dev.ID, w.shopRelease(t, "c1", sliceShopFile).ID, io.Discard, nil)
	must(t, err)
	c2 := strings.Replace(
		sliceFile(t, false, false),
		"      default_access: write\n",
		"      default_access: write\n      version_days: 14\n      quota_gb: 2\n      cors_origins:\n        - https://app.example.com\n",
		1,
	)
	r := w.shopRelease(t, "c2", c2)
	p, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	for _, k := range []string{"update:api-dbversion_days", "update:api-dbquota_gb", "update:api-dbcors_origins"} {
		if !strings.Contains(kinds(p), k) {
			t.Errorf("plan = %s, lacks %s", kinds(p), k)
		}
	}
	_, err = w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
	must(t, err)
	sl, err := d.Tiles.GetBySlug(ctx, w.dev.ID, "api-db")
	must(t, err)
	if sl.VersionDays == nil || *sl.VersionDays != 14 || sl.QuotaGB == nil || *sl.QuotaGB != 2 ||
		!slices.Equal(sl.CORSOrigins, []string{"https://app.example.com"}) {
		t.Errorf("slice row = %v %v %v", sl.VersionDays, sl.QuotaGB, sl.CORSOrigins)
	}
}

// keep (the default): the database stays on the instance, the rows go, and
// the next plan of the same file is clean.
func TestSliceKeep(t *testing.T) {
	w := newSliceWorld(t)
	d := w.f.D
	_, err := w.f.Apply(ctx, w.dev.ID, w.shopRelease(t, "c1", sliceFile(t, true, false)).ID, io.Discard, nil)
	must(t, err)
	sl, err := d.Tiles.GetBySlug(ctx, w.dev.ID, "api-db")
	must(t, err)
	pr, ok, err := d.Operated.ProvisionOf(ctx, sl.ID)
	if err != nil || !ok || sl.OnRemove == nil || *sl.OnRemove != tile.Keep {
		t.Fatalf("provision = %+v %v %v, on_remove %v", pr, ok, err, sl.OnRemove)
	}
	n := len(w.fake.Calls())
	r := w.shopRelease(t, "c2", sliceFile(t, true, true))
	_, err = w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
	must(t, err)
	if ex := execs(w, n); strings.Contains(ex, "DROP DATABASE") || !strings.Contains(ex, `DROP ROLE IF EXISTS "shop_dev_api_db_api"`) {
		t.Errorf("keep execs:\n%s", ex)
	}
	if _, err := d.Operated.GetProvision(ctx, pr.ID); err == nil {
		t.Error("the kept slice kept its provision row")
	}
	p, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	if len(p.Changes) != 0 || p.Blocked() {
		t.Errorf("re-plan = %s, blockers %q, want clean", kinds(p), p.Blockers)
	}
}

// A slice tile removed on its own: reporter's slice_access stops naming it.
func TestSliceRemoveForgetsAccess(t *testing.T) {
	w := newSliceWorld(t)
	d := w.f.D
	_, err := w.f.Apply(ctx, w.dev.ID, w.shopRelease(t, "c1", sliceShopFile).ID, io.Discard, nil)
	must(t, err)
	sl, err := d.Tiles.GetBySlug(ctx, w.dev.ID, "api-db")
	must(t, err)
	rep, err := d.Tiles.GetBySlug(ctx, w.dev.ID, "reporter")
	must(t, err)
	if len(rep.SliceAccess) != 1 {
		t.Fatalf("reporter slice_access before = %v", rep.SliceAccess)
	}
	_, err = w.f.Remove(ctx, w.dev, []store.Tile{sl}, io.Discard)
	must(t, err)
	rep, err = d.Tiles.Get(ctx, rep.ID)
	must(t, err)
	if len(rep.SliceAccess) != 0 {
		t.Errorf("reporter slice_access after = %v, want api-db gone", rep.SliceAccess)
	}
}

// The file drops api-db but api still refs it: a file blocker, not a
// dropped database under a running consumer (loop 4, F13).
func TestSliceGoneWhileReffedBlocks(t *testing.T) {
	w := newSliceWorld(t)
	_, err := w.f.Apply(ctx, w.dev.ID, w.shopRelease(t, "c1", sliceShopFile).ID, io.Discard, nil)
	must(t, err)
	block := "    api-db:\n      kind: slice\n      provision_from: infra:${{ env.name }}:pg-db\n      default_access: write\n"
	if !strings.Contains(sliceShopFile, block) {
		t.Fatal("no api-db block in sliceShopFile")
	}
	p, err := w.f.Plan(ctx, w.dev.ID, w.shopRelease(t, "c2", strings.Replace(sliceShopFile, block, "", 1)).ID, io.Discard)
	must(t, err)
	want := "environment dev tile api: refs ${{ tile.api-db.url }}: no tile of that name in this environment"
	if len(p.Blockers) != 1 || !strings.HasSuffix(p.Blockers[0], want) {
		t.Errorf("blockers = %q, want one ending %q", p.Blockers, want)
	}
}

// A slice names its url setting; a name no engine publishes (an old
// uppercase one) and a bad url word are blockers.
func TestSliceURLAndRefNames(t *testing.T) {
	w := newSliceWorld(t)
	block := "    api-db:\n      kind: slice\n      provision_from: infra:${{ env.name }}:pg-db\n      default_access: write\n"
	if !strings.Contains(sliceShopFile, block) {
		t.Fatal("no api-db block in sliceShopFile")
	}
	pooled := strings.Replace(sliceShopFile, block, block+"      url: pooled\n", 1)
	_, err := w.f.Apply(ctx, w.dev.ID, w.shopRelease(t, "c1", pooled).ID, io.Discard, nil)
	must(t, err)
	row, err := w.f.D.Tiles.GetBySlug(ctx, w.dev.ID, "api-db")
	must(t, err)
	if row.SliceURL == nil || *row.SliceURL != "pooled" {
		t.Errorf("api-db url = %v, want pooled", row.SliceURL)
	}
	for name, c := range map[string]struct{ file, want string }{
		"old name": {strings.Replace(sliceShopFile, "tile.api-db.url", "tile.api-db.DATABASE_URL", 1), `a slice has no output "DATABASE_URL"; valid: postgres: `},
		"bad url":  {strings.Replace(sliceShopFile, block, block+"      url: both\n", 1), `url "both" must be direct or pooled`},
	} {
		p, err := w.f.Plan(ctx, w.dev.ID, w.shopRelease(t, "c-"+strings.ReplaceAll(name, " ", ""), c.file).ID, io.Discard)
		if err == nil {
			err = errors.New(strings.Join(p.Blockers, "; "))
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

// A file that drops every tile applies: the rollout of an empty env is an
// empty order, not "depends_on has a cycle" (loop 4, F14).
func TestApplyEmptyFile(t *testing.T) {
	w := newSliceWorld(t)
	_, err := w.f.Apply(ctx, w.dev.ID, w.shopRelease(t, "c1", sliceShopFile).ID, io.Discard, nil)
	must(t, err)
	var log bytes.Buffer
	_, err = w.f.Apply(ctx, w.dev.ID, w.shopRelease(t, "c2", "version: 1\nstack: shop\nladder:\n  - dev\n  - prd\nhead: main\n").ID, &log, nil)
	must(t, err)
	if live, _ := w.f.D.Tiles.List(ctx, w.dev.ID); len(live) != 0 {
		t.Errorf("tiles after the empty file = %d, want 0", len(live))
	}
	if !strings.Contains(log.String(), "plan: delete api\n") {
		t.Errorf("job log = %q, want the plan rows", log.String())
	}
}

// A second plan of the same release is clean: reporter, a cron built from
// git with no build: block, used to show build_context and dockerfile rows
// every time (the stored defaults against the file's blanks).
func TestCronBuildReplansClean(t *testing.T) {
	w := newSliceWorld(t)
	r := w.shopRelease(t, "c1", sliceShopFile)
	_, err := w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
	must(t, err)
	p, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	if len(p.Changes) != 0 || p.Blocked() {
		t.Errorf("re-plan = %s, blockers %q, want clean", kinds(p), p.Blockers)
	}
}

// api stops reffing api-db: its user goes only once its new replicas are
// up, so a failed rollout leaves the old ones their cred.
func TestSliceUnbindAfterSwap(t *testing.T) {
	w := newSliceWorld(t)
	d := w.f.D
	_, err := w.f.Apply(ctx, w.dev.ID, w.shopRelease(t, "c1", sliceShopFile).ID, io.Discard, nil)
	must(t, err)
	api, err := d.Tiles.GetBySlug(ctx, w.dev.ID, "api")
	must(t, err)
	ref := "      env:\n        DATABASE_URL: ${{ tile.api-db.url }}\n"
	if !strings.Contains(sliceShopFile, ref) {
		t.Fatal("no api ref in sliceShopFile")
	}
	w.fake.Err = map[string]error{
		"Run": errors.New("no room"),
	}
	_, err = w.f.Apply(ctx, w.dev.ID, w.shopRelease(t, "c2", strings.Replace(sliceShopFile, ref, "", 1)).ID, io.Discard, nil)
	if err == nil {
		t.Fatal("apply with a failing rollout succeeded")
	}
	if bound, _ := d.Operated.Bound(ctx, api.ID); len(bound) != 1 {
		t.Fatalf("after a failed rollout bindings = %v, want api's kept", bound)
	}
	w.fake.Err = nil
	n := len(w.fake.Calls())
	must(t, d.Redeploy(ctx, api.ID, io.Discard, nil))
	if ex := execs(w, n); !strings.Contains(ex, `DROP ROLE IF EXISTS "shop_dev_api_db_api"`) {
		t.Errorf("redeploy execs:\n%s", ex)
	}
	if bound, _ := d.Operated.Bound(ctx, api.ID); len(bound) != 0 {
		t.Errorf("after the swap bindings = %v, want none", bound)
	}
}

// An instance still holding a slice is refused and left as it was.
func TestSliceInstanceRemovalRefused(t *testing.T) {
	w := newSliceWorld(t)
	d := w.f.D
	_, err := w.f.Apply(ctx, w.dev.ID, w.shopRelease(t, "c1", sliceShopFile).ID, io.Discard, nil)
	must(t, err)
	n := len(w.fake.Calls())
	_, err = w.f.Remove(ctx, w.envs["staging"], []store.Tile{w.pg["staging"]}, io.Discard)
	if _, ok := errs.IsConflict(err); !ok {
		t.Fatalf("remove = %v, want a conflict", err)
	}
	if _, err := d.Operated.GetByTile(ctx, w.pg["staging"].ID); err != nil {
		t.Errorf("instance row after a refusal: %v", err)
	}
	for _, c := range w.fake.Calls()[n:] {
		if c.Method != "Exec" {
			t.Errorf("a refusal touched docker: %s %v", c.Method, c.Args)
		}
	}
}

// An allow list narrowed after the plan: the consumer's next deploy fails
// with the plan's own reason and its binding stays.
func TestSliceNotAllowedAtDeploy(t *testing.T) {
	w := newSliceWorld(t)
	d := w.f.D
	_, err := w.f.Apply(ctx, w.dev.ID, w.shopRelease(t, "c1", sliceShopFile).ID, io.Discard, nil)
	must(t, err)
	w.instance(t, "staging", func(m *store.OperatedInstance) {
		m.Allow = store.StringList{"testorg:blog:*"}
	})
	api, err := d.Tiles.GetBySlug(ctx, w.dev.ID, "api")
	must(t, err)
	runs := len(w.fake.Specs)
	err = d.Redeploy(ctx, api.ID, io.Discard, nil)
	want := "slice api-db: infra/staging/pg-db does not allow testorg:shop:dev:api-db; add it to the Allow list of pg-db"
	if err == nil || err.Error() != want {
		t.Fatalf("redeploy = %v, want %q", err, want)
	}
	if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("redeploy error is %T, want a conflict", err)
	}
	if len(w.fake.Specs) != runs {
		t.Error("a refused consumer started a container")
	}
	bound, err := d.Operated.Bound(ctx, api.ID)
	must(t, err)
	if len(bound) != 1 {
		t.Errorf("bindings = %v, want api's kept", bound)
	}
}

// Infra's own promote: allow and env_pairs are plan rows written onto the
// instance, never a redeploy.
func TestOperatedAllowRows(t *testing.T) {
	w := newSliceWorld(t)
	w.instance(t, "staging", func(m *store.OperatedInstance) {
		m.Allow, m.EnvPairs = nil, nil
	})
	rel := func(commit, file string) store.Release {
		w.files[commit] = file
		r, err := w.f.D.Releases.Create(ctx, w.infra.ID, "test", []release.Pin{
			{
				Slug:      release.ConfigSlug,
				Repo:      w.infra.ConfigRepo,
				CommitSHA: commit,
			},
		})
		must(t, err)
		return r
	}
	staging := w.envs["staging"].ID
	p, err := w.f.Apply(ctx, staging, rel("i1", infraFile).ID, io.Discard, nil)
	must(t, err)
	got := kinds(p)
	for _, want := range []string{
		"operated:pg-dballow+testorg:blog:*:api +testorg:shop:*",
		"operated:pg-dbenv_pairs+dev→staging +production→production +staging→staging",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan = %s, want %s", got, want)
		}
	}
	if len(p.Deployed) != 0 {
		t.Errorf("deployed %v; an allow edit redeploys nothing", p.Deployed)
	}
	m, err := w.f.D.Operated.GetByTile(ctx, w.pg["staging"].ID)
	must(t, err)
	if len(m.Allow) != 2 || m.EnvPairs["dev"] != "staging" {
		t.Errorf("instance = %+v", m)
	}

	narrowed := strings.Replace(infraFile, "        - testorg:shop:*\n", "", 1)
	p, err = w.f.Plan(ctx, staging, rel("i2", narrowed).ID, io.Discard)
	must(t, err)
	if got := kinds(p); got != "operated:pg-dballow−testorg:shop:*" {
		t.Errorf("plan = %s, want only the removed entry", got)
	}
}
