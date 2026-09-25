package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

type vipStub struct{}

func (vipStub) Set(context.Context, string, []string) error {
	return nil
}

func (vipStub) Remove(context.Context, string) error {
	return nil
}

type world struct {
	orch   *Orchestrator
	fake   *dockerfake.Fake
	st     *store.Store
	mu     sync.Mutex
	pushed []string
	org    string
	stack  string
	env    string
}

func (w *world) lastPush() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pushed) == 0 {
		return ""
	}
	return w.pushed[len(w.pushed)-1]
}

// newWorld: an orchestrator on the fake daemon with an org, a stack and a
// dev env seeded.
func newWorld(t *testing.T) *world {
	t.Helper()
	dir := t.TempDir()
	w := &world{fake: dockerfake.New()}
	orch, err := New(
		Config{DataDir: dir, SecretsKey: testKey, Conntrack: dir + "/nf_conntrack"},
		WithDocker(w.fake),
		WithVIP(vipStub{}),
		WithProxy(func(_ context.Context, cfg json.RawMessage) error {
			w.mu.Lock()
			w.pushed = append(w.pushed, string(cfg))
			w.mu.Unlock()
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = orch.Close() })
	w.orch, w.st = orch, storetest.Open(t, filepath.Join(dir, "stackr.db"))
	ctx := context.Background()
	now := time.Now()
	w.org, w.stack, w.env = uuid.NewString(), uuid.NewString(), uuid.NewString()
	must(t, w.st.Orgs.Create(ctx, store.Org{
		ID:        w.org,
		Name:      "acme",
		Slug:      "acme",
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, w.st.Stacks.Create(ctx, store.Stack{
		ID:        w.stack,
		OrgID:     w.org,
		Name:      "shop",
		Slug:      "shop",
		Settings:  "{}",
		Domains:   "[]",
		CreatedAt: now,
	}))
	must(t, w.st.Environments.Create(ctx, store.Environment{
		ID:         w.env,
		StackID:    w.stack,
		Name:       "dev",
		Slug:       "dev",
		Type:       "static",
		Settings:   "{}",
		Network:    "n",
		FromKind:   "branch",
		FromBranch: "main",
		CreatedAt:  now,
	}))
	return w
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (w *world) tile(t *testing.T, name string, running bool) Tile {
	t.Helper()
	tl, err := w.orch.CreateTile(context.Background(), Tile{
		StackID:       w.stack,
		EnvironmentID: w.env,
		Name:          name,
		Kind:          tile.Image,
		ImageRef:      "nginx:1",
		ContainerPort: 80,
	})
	must(t, err)
	if running {
		w.fake.Containers = append(w.fake.Containers, docker.Container{
			ID:     "c-" + name,
			Name:   "c-" + name,
			State:  "running",
			Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"},
		})
	}
	return tl
}

// wait polls a job until it finishes.
func (w *world) wait(t *testing.T, id string) Job {
	t.Helper()
	for range 500 {
		j, err := w.orch.GetJob(context.Background(), id)
		must(t, err)
		if j.State == job.Done || j.State == job.Failed || j.State == job.Cancelled {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never finished", id)
	return Job{}
}

// B34: a param change on a running tile queues its redeploy (the release
// image, flow/deploy's Redeploy); a stopped tile is left alone.
func TestParamChangeRedeploysRunningTiles(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	up := w.tile(t, "api", true)
	down := w.tile(t, "worker", false)
	must(t, w.orch.SetParams(ctx, ParamScope{Kind: "stack", ID: w.stack}, []ParamEntry{
		{
			Collection: "app",
			Name:       "mode",
			Kind:       "param",
			Value:      "fast",
		},
	}))
	js, err := w.orch.TileJobs(ctx, []string{up.ID, down.ID}, 10)
	must(t, err)
	if len(js) != 1 || js[0].Kind != string(kindDeploy) || !slices.Contains(js[0].LockSet, up.ID) {
		t.Fatalf("jobs after a param change = %+v, want one deploy of %s", js, up.Slug)
	}
}

// B20, B2: the dry run's blockers are the ones the promote job refuses
// with, and a rollback takes the same path.
func TestPromoteBlockersOneAnswer(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	other := uuid.NewString()
	must(t, w.st.Stacks.Create(ctx, store.Stack{
		ID:        other,
		OrgID:     w.org,
		Name:      "blog",
		Slug:      "blog",
		Settings:  "{}",
		Domains:   "[]",
		CreatedAt: time.Now(),
	}))
	rel, err := w.orch.releases.Create(ctx, other, "test", nil)
	must(t, err)

	pp, err := w.orch.PlanPromote(ctx, w.env, rel.ID)
	must(t, err)
	if pp.CanDeploy || len(pp.Plan.Blockers) != 1 {
		t.Fatalf("plan = %+v, want one blocker", pp)
	}
	for _, verb := range []func(context.Context, string, string) (Job, error){w.orch.Promote, w.orch.Rollback} {
		j, err := verb(ctx, w.env, rel.ID)
		must(t, err)
		if j = w.wait(t, j.ID); j.State != job.Failed || !strings.Contains(j.Error, pp.Plan.Blockers[0]) {
			t.Errorf("job = %s %q, want failed with %q", j.State, j.Error, pp.Plan.Blockers[0])
		}
	}
}

// A domain write pushes a whole config carrying Caddy's admin listener.
func TestDomainPushesProxy(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	api := w.tile(t, "api", true)
	_, err := w.orch.AttachDomain(ctx, api.ID, DomainSpec{Host: "api.example.com"})
	must(t, err)
	cfg := w.lastPush()
	if !strings.Contains(cfg, "api.example.com") || !strings.Contains(cfg, ProxyAdmin) {
		t.Errorf("pushed config lacks the host or the admin listener:\n%s", cfg)
	}
}

// B1, B21: UpdateTile edits the stored row; untouched fields survive.
func TestUpdateTileKeepsUntouchedFields(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	api := w.tile(t, "api", false)
	got, _, err := w.orch.UpdateTile(ctx, api.ID, func(t *Tile) error {
		t.HealthPath = "/up"
		return nil
	})
	must(t, err)
	if got.HealthPath != "/up" || got.ImageRef != "nginx:1" || got.ContainerPort != 80 {
		t.Errorf("after update: %+v", got)
	}
}

// B25: every deploy is a job row the caller polls; the verb returns before
// the work and the row finishes on its own.
func TestDeployIsAJobRow(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tl := w.tile(t, "api", false)
	j, err := w.orch.Deploy(ctx, tl.ID)
	must(t, err)
	if j.ID == "" || j.Kind != string(kindDeploy) || !slices.Contains(j.LockSet, tl.ID) {
		t.Fatalf("Deploy returned %+v, want a deploy row for %s", j, tl.ID)
	}
	if got := w.wait(t, j.ID); got.ID != j.ID {
		t.Fatalf("polled %s, got %s", j.ID, got.ID)
	}
}

// A trusted_proxies value Caddy would reject is refused before it can break
// every proxy push.
func TestTrustedProxiesRefusesNonCIDR(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := w.orch.SetSetting(ctx, "trusted_proxies", "10.0.0.0/8, cloudflare"); err == nil {
		t.Fatal("cloudflare accepted as a CIDR")
	}
	must(t, w.orch.SetSetting(ctx, "trusted_proxies", "10.0.0.0/8, 192.168.1.1"))
	if !strings.Contains(w.lastPush(), `"10.0.0.0/8"`) {
		t.Errorf("push lacks the range: %s", w.lastPush())
	}
}

const nightly = "    nightly:\n      kind: cron\n      image: busybox:1\n      schedule: \"0 3 * * *\"\n      command: \"true\"\n"

// promoteCron promotes a stack file holding one image cron, nightly, into
// dev and returns the tile.
func (w *world) promoteCron(t *testing.T) Tile { return w.promoteTile(t, "nightly", nightly) }

// promoteTile promotes a stack file whose base holds tiles (yaml) into dev
// and returns the tile slug.
func (w *world) promoteTile(t *testing.T, slug, tiles string) Tile {
	t.Helper()
	w.fake.Digests = map[string]string{"busybox:1": "sha256:b1"}
	ctx := context.Background()
	st, err := w.st.Stacks.Get(ctx, w.stack)
	must(t, err)
	st.ConfigRepo, st.ConfigBranch = "https://github.com/acme/shop", "main"
	must(t, w.st.Stacks.Update(ctx, st))
	w.orch.promote.Config = func(context.Context, store.Stack, string, io.Writer) ([]byte, promote.Fetcher, error) {
		return []byte("version: 1\nstack: shop\nladder: [dev]\nhead: main\nbase:\n  tiles:\n" + tiles), nil, nil
	}
	rel, err := w.orch.releases.Create(ctx, w.stack, "test",
		[]release.Pin{{Slug: release.ConfigSlug, Repo: st.ConfigRepo, CommitSHA: "c1"}})
	must(t, err)
	j, err := w.orch.Promote(ctx, w.env, rel.ID)
	must(t, err)
	if j = w.wait(t, j.ID); j.State != job.Done {
		t.Fatalf("promote = %s: %s", j.State, j.Error)
	}
	tl, err := w.orch.tiles.GetBySlug(ctx, w.env, slug)
	must(t, err)
	return tl
}

// Step 3b: a function with trigger on_deploy gets one run per deploy.
func TestOnDeployRuns(t *testing.T) {
	w := newWorld(t)
	fn := w.promoteTile(t, "migrate",
		"    migrate:\n      kind: function\n      image: busybox:1\n      trigger: on_deploy\n")
	rs, err := w.orch.Runs(context.Background(), fn.ID, 0)
	must(t, err)
	if len(rs) != 1 || rs[0].Trigger != "deploy" {
		t.Fatalf("runs after promote = %+v, want one deploy run", rs)
	}
	j := w.wait(t, rs[0].JobID)
	if r, _ := w.orch.Run(context.Background(), fn.ID, rs[0].ID); j.State != job.Done || r.Status != "ok" {
		t.Fatalf("on_deploy run = %s / %+v", j.State, r)
	}
}

// Step 3b: a promote of a stack file with a cron tile registers its
// schedule entry; pausing the tile takes the entry out.
func TestPromoteRegistersCron(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	cron := w.promoteCron(t)
	if !slices.Contains(w.orch.sched.Names(), "cron "+cron.ID) {
		t.Fatalf("entries after promote = %v, want cron %s", w.orch.sched.Names(), cron.ID)
	}
	_, err := w.orch.PauseTile(ctx, cron.ID, true)
	must(t, err)
	if slices.Contains(w.orch.sched.Names(), "cron "+cron.ID) {
		t.Fatalf("entries after pause = %v, want no cron entry", w.orch.sched.Names())
	}
	st, err := w.orch.TileStatus(ctx, cron.ID)
	must(t, err)
	if !st.Paused || st.NextRun != nil {
		t.Fatalf("status of a paused cron = %+v, want paused and no next run", st)
	}
	_, err = w.orch.PauseTile(ctx, cron.ID, false)
	must(t, err)
	if st, _ = w.orch.TileStatus(ctx, cron.ID); st.Paused || st.NextRun == nil {
		t.Fatalf("status after resume = %+v, want a next run", st)
	}
}

// Step 3b verbs: a manual run, the overlap refusal, StopRun (ownership
// first), and the Stop/Restart guards.
func TestRunVerbs(t *testing.T) {
	w := newWorld(t)
	w.fake.WaitBlock = true // a run goes until it is stopped
	ctx := context.Background()
	cron := w.promoteCron(t)
	other := w.tile(t, "api", false)

	j, r, err := w.orch.RunTile(ctx, cron.ID)
	must(t, err)
	if j.ID == "" || r.JobID != j.ID || r.Trigger != "manual" {
		t.Fatalf("RunTile = %+v, %+v", j, r)
	}
	for range 500 {
		if r, _ = w.orch.Run(ctx, cron.ID, r.ID); r.Status == "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.Status != "running" {
		t.Fatalf("run = %+v, want running", r)
	}
	j2, r2, err := w.orch.RunTile(ctx, cron.ID)
	must(t, err)
	if j2.ID != "" || r2.Status != "cancelled" || r2.Reason != "previous run still going" {
		t.Fatalf("overlapping run = %+v, %+v", j2, r2)
	}
	if err := w.orch.StopRun(ctx, other.ID, r.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("StopRun through another tile = %v, want not found", err)
	}
	must(t, w.orch.StopRun(ctx, cron.ID, r.ID))
	if j = w.wait(t, j.ID); j.State != job.Cancelled {
		t.Fatalf("run job after StopRun = %s", j.State)
	}
	if r, _ = w.orch.Run(ctx, cron.ID, r.ID); r.Status != "cancelled" || r.Reason != "stopped" {
		t.Fatalf("run after StopRun = %+v", r)
	}
	if rs, _ := w.orch.Runs(ctx, cron.ID, 0); len(rs) != 2 {
		t.Fatalf("runs = %d, want 2", len(rs))
	}
	st, err := w.orch.TileStatus(ctx, cron.ID)
	must(t, err)
	if st.LastRun == nil || st.NextRun == nil {
		t.Fatalf("status = %+v, want a last and a next run", st)
	}

	msg := func(err error) string {
		v, _ := errs.IsInvalid(err)
		return v.Msg
	}
	if _, err := w.orch.RestartTile(ctx, cron.ID); msg(err) != "a cron has no long-running container to restart; use run instead" {
		t.Fatalf("restart a cron = %v", err)
	}
	if _, err := w.orch.StartTile(ctx, cron.ID); msg(err) != "a cron has no long-running container to start; use run instead" {
		t.Fatalf("start a cron = %v", err)
	}
	if _, _, err := w.orch.RunTile(ctx, other.ID); msg(err) != "run applies to cron and function tiles" {
		t.Fatalf("run an image tile = %v", err)
	}
	if _, err := w.orch.StopTile(ctx, cron.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ = w.orch.TileStatus(ctx, cron.ID); !st.Paused {
		t.Fatal("stop on a cron did not pause it")
	}
}

// The drawer reads: Routes names each domain's tile, TileVolumes finds the
// env volumes a tile mounts and no other.
func TestDrawerReads(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	api := w.tile(t, "api", true)
	_, err := w.orch.AttachDomain(ctx, api.ID, DomainSpec{Host: "api.example.com"})
	must(t, err)
	rs, err := w.orch.Routes(ctx, w.env)
	must(t, err)
	if len(rs) != 1 || rs[0].Host != "api.example.com" || rs[0].Tile != "api" {
		t.Errorf("routes = %+v", rs)
	}
	scope := VolumeScope{Kind: "env", ID: w.env}
	_, err = w.orch.DeclareVolume(ctx, scope, "uploads", 0)
	must(t, err)
	_, err = w.orch.DeclareVolume(ctx, scope, "cache", 0)
	must(t, err)
	_, _, err = w.orch.UpdateTile(ctx, api.ID, func(t *Tile) error {
		t.Volumes = "uploads:/data"
		return nil
	})
	must(t, err)
	vs, err := w.orch.TileVolumes(ctx, api.ID)
	must(t, err)
	if len(vs) != 1 || vs[0].Slug != "uploads" {
		t.Errorf("tile volumes = %+v", vs)
	}
}
