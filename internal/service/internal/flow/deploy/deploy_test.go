package deploy_test

import (
	"context"
	"io"
	"slices"
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
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

type vipStub struct{}

func (vipStub) Set(context.Context, string, []string) error {
	return nil
}

func (vipStub) Remove(context.Context, string) error {
	return nil
}

type world struct {
	f    *deploy.Flow
	st   *store.Store
	fake *dockerfake.Fake
	env  store.Environment
	tile store.Tile
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// setup: one org, stack, env ("n" network) and an image tile "api" with an
// old replica "old" and its pause container "p" already running.
func setup(t *testing.T) *world {
	st := servicetest.Store(t)
	fake := dockerfake.New()
	tiles := tile.New(st.Tiles, fake, vipStub{})
	tiles.Gate = tile.Gate{Poll: time.Millisecond, Grace: 5 * time.Millisecond, Deadline: 30 * time.Millisecond}
	f := &deploy.Flow{
		Tiles:    tiles,
		Envs:     environment.New(st.Environments, fake),
		Stacks:   stack.New(st.Stacks),
		Orgs:     org.New(st.Orgs, st.OrgMembers, st.Invites),
		Volumes:  volume.New(st.Volumes, fake),
		Images:   image.New(st.Images, fake),
		Releases: release.New(st.Releases, st.ReleaseTiles),
		Params:   params.New(st.Params),
		Managed:  managed.New(st.ManagedInstances, st.Provisions),
		Domains:  domain.New(st.Domains, fake, "proxy"),
		Creds:    credential.New(st.Credentials),
		Settings: settings.New(st.Settings, nil),
		Jobs:     job.New(st.Jobs),
	}
	o, s, e := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{
		ID:        o,
		Name:      "acme",
		Slug:      "acme",
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, st.Stacks.Create(ctx, store.Stack{
		ID:        s,
		OrgID:     o,
		Name:      "shop",
		Slug:      "shop",
		Settings:  "{}",
		CreatedAt: now,
	}))
	env := store.Environment{
		ID:         e,
		StackID:    s,
		Name:       "dev",
		Slug:       "dev",
		Type:       "static",
		Settings:   "{}",
		Network:    "n",
		FromKind:   "branch",
		FromBranch: "main",
		CreatedAt:  now,
	}
	must(t, st.Environments.Create(ctx, env))
	api, err := tiles.Create(ctx, store.Tile{
		StackID:       s,
		EnvironmentID: e,
		Name:          "api",
		Kind:          tile.Image,
		ImageRef:      "nginx:1",
		ContainerPort: 80,
	})
	must(t, err)
	lbl := func(role string) map[string]string {
		return map[string]string{tile.LabelTile: api.ID, tile.LabelRole: role}
	}
	fake.Containers = []docker.Container{
		{ID: "old", Labels: lbl("replica")},
		{ID: "p", Labels: lbl("pause")},
	}
	fake.RunIDs = []string{"new"}
	fake.Details = map[string]docker.Detail{
		"new": {Running: true, Health: "healthy", Networks: map[string]string{"n": "10.0.0.5"}},
		"old": {Running: true, Networks: map[string]string{"n": "10.0.0.4"}},
		"p":   {Running: true, Networks: map[string]string{"n": "10.0.0.2"}},
	}
	return &world{
		f:    f,
		st:   st,
		fake: fake,
		env:  env,
		tile: api,
	}
}

// at is the index of the first call matching method and first arg, or -1.
func at(calls []dockerfake.Call, method, arg string) int {
	return slices.IndexFunc(calls, func(c dockerfake.Call) bool {
		return c.Method == method && (arg == "" || (len(c.Args) > 0 && c.Args[0] == arg))
	})
}

func TestOverlapRollout(t *testing.T) {
	w := setup(t)
	if _, err := w.f.Run(ctx, w.tile, "nginx@sha256:aa", io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	c := w.fake.Calls()
	run, gone := at(c, "Run", ""), at(c, "StopRemove", "old")
	if run < 0 || gone < 0 || gone < run {
		t.Fatalf("want the new replica up before the old one goes: %v", c)
	}
	s := w.fake.Specs[0]
	if s.Image != "nginx@sha256:aa" || s.Restart != "unless-stopped" || s.Networks[0].Name != "n" ||
		len(s.Networks[0].Aliases) != 0 {
		t.Errorf("spec = %+v", s)
	}
}

func TestStopThenStartWithAMount(t *testing.T) {
	w := setup(t)
	_, _, err := w.f.Volumes.Declare(ctx, volume.Scope{Kind: "env", ID: w.env.ID}, "data", 0, nil)
	must(t, err)
	w.tile.Volumes = "data:/var/lib/data"
	if _, err := w.f.Run(ctx, w.tile, "nginx@sha256:aa", io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	c := w.fake.Calls()
	run, gone := at(c, "Run", ""), at(c, "StopRemove", "old")
	if gone < 0 || run < gone {
		t.Fatalf("want the old replica gone before the new one runs: %v", c)
	}
	if b := w.fake.Specs[0].Volumes; len(b) != 1 || b[0][len(b[0])-len(":/var/lib/data"):] != ":/var/lib/data" {
		t.Errorf("binds = %v", b)
	}
}

func TestGateFailureKeepsOld(t *testing.T) {
	w := setup(t)
	w.fake.Details["new"] = docker.Detail{Running: true, Health: "unhealthy"}
	if _, err := w.f.Run(ctx, w.tile, "nginx@sha256:aa", io.Discard, nil); err == nil {
		t.Fatal("want the gate to fail")
	}
	c := w.fake.Calls()
	if at(c, "StopRemove", "old") >= 0 || at(c, "StopRemove", "new") < 0 {
		t.Errorf("want new removed and old kept: %v", c)
	}
}

func TestParkOnUnset(t *testing.T) {
	w := setup(t)
	w.tile.EnvJSON = `{"DB_URL":"${{ params.db.url }}"}`
	_, err := w.f.Run(ctx, w.tile, "nginx@sha256:aa", io.Discard, nil)
	if u, ok := errs.IsUnset(err); !ok || u.Param != "db.url" {
		t.Fatalf("err = %v, want parked on db.url", err)
	}
	if at(w.fake.Calls(), "Run", "") >= 0 {
		t.Error("a parked tile must not start")
	}

	// A dependency parked on a param parks this tile on the same name.
	w2 := setup(t)
	db, err := w2.f.Tiles.Create(ctx, store.Tile{
		StackID:       w2.tile.StackID,
		EnvironmentID: w2.env.ID,
		Name:          "db",
		Kind:          tile.Image,
		ImageRef:      "postgres:16",
	})
	must(t, err)
	j, err := w2.f.Jobs.Create(ctx, "deploy", job.LockSet(db.ID), "{}", nil, t.TempDir())
	must(t, err)
	must(t, w2.f.Jobs.Park(ctx, j, "db.password"))
	w2.tile.DependsOn = "db"
	_, err = w2.f.Run(ctx, w2.tile, "nginx@sha256:aa", io.Discard, nil)
	if u, ok := errs.IsUnset(err); !ok || u.Param != "db.password" {
		t.Fatalf("err = %v, want parked on the dependency's db.password", err)
	}
}

// B34: a redeploy runs the image the env's release pins, never the tile's
// tag or a branch head. B29: it goes through the same gate as a deploy.
func TestRedeployRunsTheReleaseImage(t *testing.T) {
	w := setup(t)
	r, err := w.f.Releases.Create(
		ctx,
		w.tile.StackID,
		"test",
		[]release.Pin{{Slug: "api", Repo: "nginx:1", Digest: "sha256:pinned"}},
	)
	must(t, err)
	_, err = w.f.Envs.SetRelease(ctx, w.env, r.ID)
	must(t, err)
	must(t, w.f.Redeploy(ctx, w.tile.ID, io.Discard, nil))
	if got := w.fake.Specs[0].Image; got != "nginx@sha256:pinned" {
		t.Errorf("redeploy ran %q, want the pinned digest", got)
	}

	w.fake.RunIDs = []string{"new"}
	w.fake.Details["new"] = docker.Detail{Running: true, RestartCount: 1}
	if err := w.f.Redeploy(ctx, w.tile.ID, io.Discard, nil); err == nil {
		t.Error("B29: a crash-looping redeploy passed the gate")
	}
}

// An image tile's first run pins the tag's digest as a release.
func TestFirstImageRunPinsARelease(t *testing.T) {
	w := setup(t)
	w.fake.Digests = map[string]string{"nginx:1": "sha256:now"}
	must(t, w.f.Redeploy(ctx, w.tile.ID, io.Discard, nil))
	e, err := w.f.Envs.Get(ctx, w.env.ID)
	must(t, err)
	if e.ReleaseID == nil {
		t.Fatal("no release recorded")
	}
	pins, err := w.f.Releases.Pins(ctx, *e.ReleaseID)
	must(t, err)
	if p := pins["api"]; p.Digest != "sha256:now" || p.Repo != "nginx:1" {
		t.Errorf("pin = %+v", p)
	}
}

// DECIDE 140: editing an image tile's tag makes the next redeploy run the
// new tag and pin it, one release.
func TestEditedTagRedeploysAndRepins(t *testing.T) {
	w := setup(t)
	w.fake.Digests = map[string]string{"nginx:1": "sha256:one", "nginx:2": "sha256:two"}
	must(t, w.f.Redeploy(ctx, w.tile.ID, io.Discard, nil))
	cur := w.tile
	cur.ImageRef = "nginx:2"
	_, _, err := w.f.Tiles.Update(ctx, w.tile, cur)
	must(t, err)
	w.fake.RunIDs = []string{"new2"}
	w.fake.Details["new2"] = w.fake.Details["new"]
	must(t, w.f.Redeploy(ctx, w.tile.ID, io.Discard, nil))
	if got := w.fake.Specs[len(w.fake.Specs)-1].Image; got != "nginx:2" {
		t.Errorf("redeploy ran %q, want the edited tag", got)
	}
	e, err := w.f.Envs.Get(ctx, w.env.ID)
	must(t, err)
	pins, err := w.f.Releases.Pins(ctx, *e.ReleaseID)
	must(t, err)
	if p := pins["api"]; p.Digest != "sha256:two" || p.Repo != "nginx:2" {
		t.Errorf("pin = %+v, want nginx:2 at sha256:two", p)
	}
}
