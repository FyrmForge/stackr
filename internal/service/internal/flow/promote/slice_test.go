package promote

import (
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
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
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
      kind: managed
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
        DATABASE_URL: ${{ tile.api-db.DATABASE_URL }}
    reporter:
      kind: cron
      schedule: "0 3 * * *"
      slice_access:
        - from: api-db
          access: read
      env:
        DATABASE_URL: ${{ tile.api-db.DATABASE_URL }}
`

// tiles is a stack file whose base holds body (tile entries, indented four).
func tiles(body string) string {
	return "version: 1\nstack: s\nbase:\n  tiles:\n" + body
}

func TestGrammarSlices(t *testing.T) {
	r, err := Load([]byte(infraFile), nil, "testorg")
	must(t, err)
	pg := r.Envs["staging"].Tiles["pg-db"]
	if pg.Type != tile.Managed || len(pg.Allow) != 2 || pg.EnvPairs["dev"] != "staging" {
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
			"tile api: allow: only a managed tile takes it",
		},
		"env_pairs on service": {
			tiles("    api:\n      kind: service\n      env_pairs:\n        dev: staging\n"),
			"tile api: env_pairs: only a managed tile takes it",
		},
		"allow other org": {
			tiles("    db:\n      engine: postgres\n      allow:\n        - acme:*\n"),
			"tile db: allow: acme:*: the first segment must be this org, testorg",
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
		"slice_access on managed": {
			tiles("    db:\n      engine: postgres\n      slice_access:\n        - from: s\n          access: read\n"),
			"tile db: slice_access: a managed tile binds to no slice",
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
// staging and production, each with managed tile pg-db carrying the
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
		w.pg[name] = w.managedTile(t, e, []string{
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

// managedTile makes pg-db in e with its instance row.
func (w *sliceWorld) managedTile(t *testing.T, e store.Environment, allow []string, pairs map[string]string) store.Tile {
	d := w.f.D
	pg, err := d.Tiles.Create(ctx, store.Tile{
		StackID:       e.StackID,
		EnvironmentID: e.ID,
		Name:          "pg-db",
		Slug:          "pg-db",
		Kind:          tile.Managed,
	})
	must(t, err)
	m, err := d.Managed.Create(ctx, pg.ID, "postgres", "stackr", "")
	must(t, err)
	m, err = d.Managed.SetAllow(ctx, m, "testorg", allow)
	must(t, err)
	_, err = d.Managed.SetEnvPairs(ctx, m, pairs)
	must(t, err)
	return pg
}

// instance edits the instance behind infra's pg-db in env.
func (w *sliceWorld) instance(t *testing.T, env string, edit func(*store.ManagedInstance)) {
	m, err := w.f.D.Managed.GetByTile(ctx, w.pg[env].ID)
	must(t, err)
	edit(&m)
	must(t, w.s.ManagedInstances.Update(ctx, m))
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
					w.instance(t, env, func(m *store.ManagedInstance) {
						m.EnvPairs = store.StringMap{"staging": "staging"}
					})
				}
			},
			want: "slice api-db: infra's pg-db has no env pair for dev",
		},
		{
			name: "paired env missing",
			prep: func(t *testing.T, w *sliceWorld) {
				w.instance(t, "staging", func(m *store.ManagedInstance) {
					m.EnvPairs = store.StringMap{"dev": "qa"}
				})
			},
			want: "slice api-db: infra has no environment qa",
		},
		{
			name: "no env pairs, name as written",
			prep: func(t *testing.T, w *sliceWorld) {
				for _, env := range []string{"staging", "production"} {
					w.instance(t, env, func(m *store.ManagedInstance) {
						m.EnvPairs = nil
					})
				}
			},
			want: "slice api-db: infra has no environment dev",
		},
		{
			name: "no tile",
			prep: func(t *testing.T, w *sliceWorld) {
				must(t, w.f.D.Tiles.Delete(ctx, w.pg["staging"].ID))
			},
			want: "slice api-db: infra/staging has no tile pg-db",
		},
		{
			name: "not managed",
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
			want: "slice api-db: infra/staging/pg-db is not a managed tile",
		},
		{
			name: "not allowed",
			prep: func(t *testing.T, w *sliceWorld) {
				w.instance(t, "staging", func(m *store.ManagedInstance) {
					m.Allow = store.StringList{"testorg:blog:*"}
				})
			},
			want: "slice api-db: infra/staging/pg-db does not allow testorg:shop:dev:api-db",
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
	w.instance(t, "staging", func(m *store.ManagedInstance) {
		m.Allow = store.StringList{"testorg:shop:dev:*"}
	})
	p, err = w.f.Plan(ctx, pr.ID, w.shopRelease(t, "c4", sliceShopFile).ID, io.Discard)
	must(t, err)
	if want := "slice api-db: infra/staging/pg-db does not allow testorg:shop:pr-12:api-db"; !slices.Contains(p.Blockers, want) {
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
			"      env:\n        DATABASE_URL: ${{ tile.api-db.DATABASE_URL }}\n",
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
	pr, ok, err := d.Managed.ProvisionOf(ctx, sl.ID)
	must(t, err)
	m, err := d.Managed.GetByTile(ctx, w.pg["staging"].ID)
	must(t, err)
	if !ok || pr.InstanceID != m.ID || pr.DBName != "shop_dev_api_db" {
		t.Fatalf("provision = %+v %v, want shop_dev_api_db on staging's pg-db", pr, ok)
	}
	bs, err := d.Managed.Bindings(ctx, pr.ID)
	must(t, err)
	got := map[string]string{}
	for _, b := range bs {
		got[b.DBUser] = b.Access
	}
	if len(got) != 2 || got["shop_dev_api_db_api"] != "write" || got["shop_dev_api_db_reporter"] != "read" {
		t.Fatalf("bindings = %v, want api write and reporter read", got)
	}

	// Both consumers sit on the instance's network and dial its alias there.
	net := managed.Network(m.ID)
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
	if bs, _ := d.Managed.Bindings(ctx, pr.ID); len(bs) != 1 || bs[0].DBUser != "shop_dev_api_db_api" {
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
	if _, err := d.Managed.GetProvision(ctx, pr.ID); err == nil {
		t.Error("the dropped slice kept its provision")
	}
	if bs, _ := d.Managed.Bindings(ctx, pr.ID); len(bs) != 0 {
		t.Errorf("bindings after drop = %+v", bs)
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
	pr, ok, err := d.Managed.ProvisionOf(ctx, sl.ID)
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
	if _, err := d.Managed.GetProvision(ctx, pr.ID); err == nil {
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
	must(t, w.f.Remove(ctx, w.dev, []store.Tile{sl}, io.Discard))
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
	want := "environment dev tile api: refs ${{ tile.api-db.DATABASE_URL }}: no tile of that name in this environment"
	if len(p.Blockers) != 1 || !strings.HasSuffix(p.Blockers[0], want) {
		t.Errorf("blockers = %q, want one ending %q", p.Blockers, want)
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
	ref := "      env:\n        DATABASE_URL: ${{ tile.api-db.DATABASE_URL }}\n"
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
	if bound, _ := d.Managed.Bound(ctx, api.ID); len(bound) != 1 {
		t.Fatalf("after a failed rollout bindings = %v, want api's kept", bound)
	}
	w.fake.Err = nil
	n := len(w.fake.Calls())
	must(t, d.Redeploy(ctx, api.ID, io.Discard, nil))
	if ex := execs(w, n); !strings.Contains(ex, `DROP ROLE IF EXISTS "shop_dev_api_db_api"`) {
		t.Errorf("redeploy execs:\n%s", ex)
	}
	if bound, _ := d.Managed.Bound(ctx, api.ID); len(bound) != 0 {
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
	err = w.f.Remove(ctx, w.envs["staging"], []store.Tile{w.pg["staging"]}, io.Discard)
	if _, ok := errs.IsConflict(err); !ok {
		t.Fatalf("remove = %v, want a conflict", err)
	}
	if _, err := d.Managed.GetByTile(ctx, w.pg["staging"].ID); err != nil {
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
	w.instance(t, "staging", func(m *store.ManagedInstance) {
		m.Allow = store.StringList{"testorg:blog:*"}
	})
	api, err := d.Tiles.GetBySlug(ctx, w.dev.ID, "api")
	must(t, err)
	runs := len(w.fake.Specs)
	err = d.Redeploy(ctx, api.ID, io.Discard, nil)
	want := "slice api-db: infra/staging/pg-db does not allow testorg:shop:dev:api-db"
	if err == nil || err.Error() != want {
		t.Fatalf("redeploy = %v, want %q", err, want)
	}
	if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("redeploy error is %T, want a conflict", err)
	}
	if len(w.fake.Specs) != runs {
		t.Error("a refused consumer started a container")
	}
	bound, err := d.Managed.Bound(ctx, api.ID)
	must(t, err)
	if len(bound) != 1 {
		t.Errorf("bindings = %v, want api's kept", bound)
	}
}

// Infra's own promote: allow and env_pairs are plan rows written onto the
// instance, never a redeploy.
func TestManagedAllowRows(t *testing.T) {
	w := newSliceWorld(t)
	w.instance(t, "staging", func(m *store.ManagedInstance) {
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
		"managed:pg-dballow+testorg:blog:*:api +testorg:shop:*",
		"managed:pg-dbenv_pairs+dev→staging +production→production +staging→staging",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan = %s, want %s", got, want)
		}
	}
	if len(p.Deployed) != 0 {
		t.Errorf("deployed %v; an allow edit redeploys nothing", p.Deployed)
	}
	m, err := w.f.D.Managed.GetByTile(ctx, w.pg["staging"].ID)
	must(t, err)
	if len(m.Allow) != 2 || m.EnvPairs["dev"] != "staging" {
		t.Errorf("instance = %+v", m)
	}

	narrowed := strings.Replace(infraFile, "        - testorg:shop:*\n", "", 1)
	p, err = w.f.Plan(ctx, staging, rel("i2", narrowed).ID, io.Discard)
	must(t, err)
	if got := kinds(p); got != "managed:pg-dballow−testorg:shop:*" {
		t.Errorf("plan = %s, want only the removed entry", got)
	}
}
