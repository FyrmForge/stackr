package promote

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/credential"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

var ctx = context.Background()

type vipStub struct{}

func (vipStub) Set(context.Context, string, []string) error { return nil }
func (vipStub) Remove(context.Context, string) error        { return nil }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type world struct {
	f        *Flow
	fake     *dockerfake.Fake
	st       store.Stack
	dev, prd store.Environment
	files    map[string]string // commit -> stack file
}

// setup: stack "shop" with a two-rung ladder, dev (branch main) under prd
// (from promote). Config reads files[commit].
func setup(t *testing.T) *world {
	s := storetest.Store(t)
	fake := dockerfake.New()
	fake.RunID = "new"
	fake.Details = map[string]docker.Detail{"new": {Running: true, Health: "healthy", Networks: map[string]string{"n": "10.0.0.5"}}}
	tiles := tile.New(s.Tiles, fake, vipStub{})
	tiles.Gate = tile.Gate{Poll: time.Millisecond, Grace: 2 * time.Millisecond, Deadline: 30 * time.Millisecond}
	d := &deploy.Flow{
		Tiles: tiles, Envs: environment.New(s.Environments, fake), Stacks: stack.New(s.Stacks),
		Orgs: org.New(s.Orgs, s.OrgMembers, s.Invites), Volumes: volume.New(s.Volumes, fake),
		Images: image.New(s.Images, fake), Releases: release.New(s.Releases, s.ReleaseTiles),
		Params: params.New(s.Params), Managed: managed.New(s.ManagedInstances, s.Provisions),
		Domains: domain.New(s.Domains, fake, "proxy"), Creds: credential.New(s.Credentials),
		Settings: settings.New(s.Settings, nil), Jobs: job.New(s.Jobs),
	}
	w := &world{fake: fake, files: map[string]string{}}
	w.f = &Flow{D: d, Config: func(_ context.Context, _ store.Stack, commit string, _ io.Writer) ([]byte, Fetcher, error) {
		f, ok := w.files[commit]
		if !ok {
			return nil, nil, errors.New("no such commit")
		}
		return []byte(f), nil, nil
	}}
	now := time.Now()
	o := uuid.NewString()
	w.st = store.Stack{ID: uuid.NewString(), OrgID: o, Name: "shop", Slug: "shop", Settings: "{}", Domains: "[]",
		ConfigRepo: "https://github.com/acme/shop", ConfigBranch: "main", CreatedAt: now}
	must(t, s.Orgs.Create(ctx, store.Org{ID: o, Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, s.Stacks.Create(ctx, w.st))
	w.dev = store.Environment{ID: uuid.NewString(), StackID: w.st.ID, Name: "dev", Slug: "dev", Type: "static", Settings: "{}",
		Network: "n", FromKind: "branch", FromBranch: "main", Auto: true, CreatedAt: now}
	w.prd = store.Environment{ID: uuid.NewString(), StackID: w.st.ID, Name: "prd", Slug: "prd", Type: "static", Settings: "{}",
		Network: "n", Position: 1, FromKind: "promote", CreatedAt: now}
	must(t, s.Environments.Create(ctx, w.dev))
	must(t, s.Environments.Create(ctx, w.prd))
	return w
}

const shopFile = `
version: 1
stack: shop
ladder: [dev, prd]
head: main
params:
  app:
    mode: {type: param, value: fast}
    key: {type: secret}
volumes:
  data: {max_size_mb: 100}
base:
  tiles:
    api:
      image: nginx:1
      port: 80
      volumes: ["data:/srv"]
      env: {MODE: "${{ params.app.mode }}"}
      domains:
        - host: api.example.com
environments:
  dev:
    color: blue
    tiles:
      api:
        image: nginx:2
  prd: {}
`

func (w *world) release(t *testing.T, commit string, pins ...release.Pin) store.Release {
	if commit != "" {
		pins = append(pins, release.Pin{Slug: release.ConfigSlug, Repo: w.st.ConfigRepo, CommitSHA: commit})
	}
	r, err := w.f.D.Releases.Create(ctx, w.st.ID, "test", pins)
	must(t, err)
	return r
}

func kinds(p *Plan) string {
	var b []string
	for _, c := range p.Changes {
		b = append(b, c.Kind+":"+c.Tile+c.Field+c.New+c.Old)
	}
	return strings.Join(b, " ")
}

func TestGrammar(t *testing.T) {
	r, err := Load([]byte(shopFile), nil)
	must(t, err)
	dev, prd := r.Envs["dev"], r.Envs["prd"]
	if dev.Tiles["api"].Image != "nginx:2" || prd.Tiles["api"].Image != "nginx:1" || dev.Tiles["api"].Port != 80 {
		t.Errorf("overlay: dev %+v prd %+v", dev.Tiles["api"], prd.Tiles["api"])
	}
	if dev.FromKind != "branch" || dev.FromBranch != "main" || !dev.Auto || prd.FromKind != "promote" {
		t.Errorf("ladder knobs: dev %+v prd %+v", dev, prd)
	}
	if dev.Tiles["api"].Type != tile.Image {
		t.Errorf("type inference: %q", dev.Tiles["api"].Type)
	}

	inc := "version: 1\nbase:\n  tiles:\n    db: {engine: postgres}\n"
	r, err = Load([]byte("version: 1\nstack: s\ninclude: [more.yml]\n"), func(string) ([]byte, error) { return []byte(inc), nil })
	must(t, err)
	if r.Envs["production"].Tiles["db"].Type != tile.Managed {
		t.Errorf("include: %+v", r.Envs["production"].Tiles)
	}

	for name, bad := range map[string]string{
		"unknown key":    "version: 1\nstack: s\nbase:\n  tiles:\n    a: {image: x, colour: red}\n",
		"secret value":   "version: 1\nstack: s\nparams:\n  a:\n    b: {type: secret, value: x}\n",
		"cycle":          "version: 1\nstack: s\nbase:\n  tiles:\n    a: {image: x, depends_on: [b]}\n    b: {image: x, depends_on: [a]}\n",
		"shared":         "version: 1\nstack: s\nshared:\n  db: {engine: postgres}\n",
		"bottom promote": "version: 1\nstack: s\nenvironments:\n  dev: {from: promote}\n",
	} {
		if _, err := Load([]byte(bad), nil); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

// Dry run and real run agree; after applying, a re-plan is empty.
func TestPlanThenApply(t *testing.T) {
	w := setup(t)
	w.files["c1"] = shopFile
	w.fake.Digests = map[string]string{"nginx:2": "sha256:two"}
	r := w.release(t, "c1")
	p, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	if p.Blocked() {
		t.Fatalf("blocked: %v", p.Blockers)
	}
	want := "env:color env:defaults param:app.mode volume:data create:api domain:apiapi.example.com"
	if got := kinds(p); !strings.Contains(got, "create:api") || !strings.Contains(got, "volume:data") ||
		!strings.Contains(got, "param:app.mode") || !strings.Contains(got, "domain:api") {
		t.Errorf("plan = %s\nwant roughly %s", got, want)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "app.key") {
		t.Errorf("warnings = %v", p.Warnings)
	}
	applied, err := w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
	must(t, err)
	if kinds(applied) != kinds(p) {
		t.Errorf("apply planned %s, dry run %s", kinds(applied), kinds(p))
	}

	e, err := w.f.D.Envs.Get(ctx, w.dev.ID)
	must(t, err)
	pins, err := w.f.D.Releases.Pins(ctx, *e.ReleaseID)
	must(t, err)
	if pins["api"].Digest != "sha256:two" || pins[release.ConfigSlug].CommitSHA != "c1" {
		t.Errorf("pins after apply = %+v", pins)
	}
	if e.Color != "blue" {
		t.Errorf("color = %q", e.Color)
	}
	again, err := w.f.Plan(ctx, w.dev.ID, *e.ReleaseID, io.Discard)
	must(t, err)
	if len(again.Changes) != 0 || again.Blocked() {
		t.Errorf("re-plan = %s %v", kinds(again), again.Blockers)
	}

	// Dropping the tile orphans nothing it did not own and removes it.
	w.files["c2"] = strings.Replace(shopFile, "    api:\n      image: nginx:1", "    web:\n      image: nginx:1", 1)
	w.files["c2"] = strings.Replace(w.files["c2"], "      api:\n        image: nginx:2", "      web:\n        image: nginx:2", 1)
	w.files["c2"] = strings.Replace(w.files["c2"], "api.example.com", "web.example.com", 1)
	r2 := w.release(t, "c2")
	p2, err := w.f.Apply(ctx, w.dev.ID, r2.ID, io.Discard, nil)
	must(t, err)
	if got := kinds(p2); !strings.Contains(got, "delete:api") || !strings.Contains(got, "create:web") {
		t.Errorf("rename plan = %s", got)
	}
}

// B20 + B2: one rule, the same words from the dry run and the refusal;
// an older release may still land (rollback).
func TestLadderRule(t *testing.T) {
	w := setup(t)
	_, err := w.f.D.Tiles.Create(ctx, store.Tile{StackID: w.st.ID, EnvironmentID: w.prd.ID, Name: "api", Kind: tile.Image, ImageRef: "nginx:1", ContainerPort: 80})
	must(t, err)
	r1 := w.release(t, "", release.Pin{Slug: "api", Repo: "nginx", Digest: "sha256:one"})
	r2 := w.release(t, "", release.Pin{Slug: "api", Repo: "nginx", Digest: "sha256:two"})

	p, err := w.f.Plan(ctx, w.prd.ID, r1.ID, io.Discard)
	must(t, err)
	if !p.Blocked() || !strings.Contains(p.Blockers[0], "runs nothing") {
		t.Errorf("empty dev: %v", p.Blockers)
	}
	_, err = w.f.D.Envs.SetRelease(ctx, w.dev, r1.ID)
	must(t, err)

	p, err = w.f.Plan(ctx, w.prd.ID, r2.ID, io.Discard)
	must(t, err)
	_, applyErr := w.f.Apply(ctx, w.prd.ID, r2.ID, io.Discard, nil)
	if _, ok := errs.IsConflict(applyErr); !ok || !p.Blocked() || applyErr.Error() != strings.Join(p.Blockers, "; ") &&
		!strings.Contains(applyErr.Error(), p.Blockers[0]) {
		t.Errorf("ahead of dev: plan %v, apply %v", p.Blockers, applyErr)
	}

	_, err = w.f.Apply(ctx, w.prd.ID, r1.ID, io.Discard, nil)
	must(t, err)
	_, err = w.f.D.Envs.SetRelease(ctx, w.dev, r2.ID)
	must(t, err)
	_, err = w.f.Apply(ctx, w.prd.ID, r2.ID, io.Discard, nil)
	must(t, err)
	// Rollback: prd back to #1, which dev has long passed.
	p, err = w.f.Apply(ctx, w.prd.ID, r1.ID, io.Discard, nil)
	must(t, err)
	if len(p.Changes) != 1 || p.Changes[0].Kind != "image" {
		t.Errorf("rollback plan = %s", kinds(p))
	}
	got := ""
	for _, sp := range w.fake.Specs {
		if sp.Image != tile.PauseImage {
			got = sp.Image
		}
	}
	if got != "nginx@sha256:one" {
		t.Errorf("rollback ran %q", got)
	}
}

func TestFileBlockers(t *testing.T) {
	w := setup(t)
	w.files["c1"] = strings.Replace(shopFile, "api.example.com", "${{ params.web.host }}", 1)
	w.files["c1"] = strings.Replace(w.files["c1"], "      port: 80\n", "      port: 80\n      files: [\"a:/b\"]\n", 1)
	r := w.release(t, "c1")
	p, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	if !p.Blocked() || !strings.Contains(strings.Join(p.Blockers, "|"), "files:") {
		t.Errorf("blockers = %v", p.Blockers)
	}
	if _, err := w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil); err == nil {
		t.Error("a blocked promote applied")
	}
	r2 := w.release(t, "nope")
	if p, _ := w.f.Plan(ctx, w.dev.ID, r2.ID, io.Discard); !p.Blocked() {
		t.Error("an unreadable stack file did not block")
	}
}

func TestPushHelpers(t *testing.T) {
	for _, s := range []string{"https://github.com/Acme/Shop.git", "git@github.com:acme/shop.git", "acme/shop/"} {
		if got := NormalizeRepo(s); got != "acme/shop" {
			t.Errorf("NormalizeRepo(%q) = %q", s, got)
		}
	}
	cases := []struct {
		watch, changed []string
		want           bool
	}{
		{nil, []string{"x"}, true},
		{[]string{"^api/"}, nil, true},
		{[]string{"^api/"}, []string{"web/a.go"}, false},
		{[]string{"^api/", "!_test\\.go$"}, []string{"api/a_test.go"}, false},
		{[]string{"^api/", "!_test\\.go$"}, []string{"api/a_test.go", "api/a.go"}, true},
		{[]string{"!^docs/"}, []string{"main.go"}, true},
	}
	for _, c := range cases {
		if got := watchMatch(c.watch, c.changed); got != c.want {
			t.Errorf("watchMatch(%v, %v) = %v", c.watch, c.changed, got)
		}
	}
	if !configOnly([]string{DefaultPath}, "") || configOnly(nil, "") || configOnly([]string{DefaultPath, "a"}, "") {
		t.Error("configOnly")
	}
}

func TestPushBuildsAndReleases(t *testing.T) {
	w := setup(t)
	w.files["c9"] = "version: 1\nstack: shop\nladder: [dev, prd]\nhead: main\nbase:\n  tiles:\n    api: {port: 80, watch_paths: [\"^api/\"]}\n"
	var built []string
	w.f.Build = func(ctx context.Context, _ store.Stack, t store.Tile, commit string, _ io.Writer) (string, error) {
		built = append(built, t.Slug+"@"+commit)
		im, err := w.f.D.Images.Built(ctx, "stackr/api:"+commit, "")
		return im.ID, err
	}
	r, auto, err := w.f.Push(ctx, w.st.ID, Event{Repo: "git@github.com:acme/shop.git", Branch: "main", Commit: "c9",
		Changed: []string{"api/main.go"}}, io.Discard)
	must(t, err)
	if len(built) != 1 || built[0] != "api@c9" || len(auto) != 1 || auto[0].Slug != "dev" {
		t.Fatalf("built %v auto %v", built, auto)
	}
	pins, err := w.f.D.Releases.Pins(ctx, r.ID)
	must(t, err)
	if pins["api"].ImageID == nil || pins[release.ConfigSlug].CommitSHA != "c9" {
		t.Errorf("pins = %+v", pins)
	}
	built = nil
	_, _, err = w.f.Push(ctx, w.st.ID, Event{Repo: "acme/shop", Branch: "main", Commit: "c9", Changed: []string{"docs/x"}}, io.Discard)
	must(t, err)
	if len(built) != 0 {
		t.Errorf("watch paths ignored: built %v", built)
	}
}

// Step 3b grammar: kind: cron + schedule:, kind: function + trigger:; the
// plan diff names what moved.
func TestPlanShowsRunKinds(t *testing.T) {
	w := setup(t)
	file := func(sched, trigger string) string {
		return "version: 1\nstack: shop\nladder: [dev, prd]\nhead: main\nbase:\n  tiles:\n" +
			"    nightly:\n      kind: cron\n      image: busybox:1\n      schedule: \"" + sched + "\"\n      command: \"true\"\n" +
			"    migrate:\n      kind: function\n      image: busybox:1\n      trigger: " + trigger + "\n"
	}
	w.files["c1"] = file("0 3 * * *", "manual")
	w.fake.Digests = map[string]string{"busybox:1": "sha256:b1"}
	p, err := w.f.Apply(ctx, w.dev.ID, w.release(t, "c1").ID, io.Discard, nil)
	must(t, err)
	if got := kinds(p); !strings.Contains(got, "create:nightly") || !strings.Contains(got, "create:migrate") {
		t.Fatalf("first plan = %s", got)
	}
	n, err := w.f.D.Tiles.GetBySlug(ctx, w.dev.ID, "nightly")
	must(t, err)
	if n.Kind != tile.Cron || n.Schedule != "0 3 * * *" || n.TimeoutMinutes != tile.DefaultTimeout {
		t.Fatalf("cron row = %+v", n)
	}
	w.files["c2"] = file("0 4 * * *", "on_deploy")
	p, err = w.f.Plan(ctx, w.dev.ID, w.release(t, "c2").ID, io.Discard)
	must(t, err)
	got := kinds(p)
	if !strings.Contains(got, "update:nightlyschedule0 4 * * *0 3 * * *") || !strings.Contains(got, "update:migratetriggeron_deploymanual") {
		t.Fatalf("plan = %s, want the schedule and trigger moves", got)
	}
	w.files["c3"] = "version: 1\nstack: shop\nladder: [dev, prd]\nhead: main\nbase:\n  tiles:\n" +
		"    nightly:\n      kind: cron\n      type: service\n      image: busybox:1\n"
	p, err = w.f.Plan(ctx, w.dev.ID, w.release(t, "c3").ID, io.Discard)
	must(t, err)
	if !strings.Contains(strings.Join(p.Blockers, ";"), "kind: cron and type: service disagree") {
		t.Fatalf("kind and type disagreeing = %v", p.Blockers)
	}
}
