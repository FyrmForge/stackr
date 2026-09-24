package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

type vipStub struct{}

func (vipStub) Set(context.Context, string, []string) error { return nil }
func (vipStub) Remove(context.Context, string) error        { return nil }

type world struct {
	o      *Orchestrator
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
	o, err := New(Config{DataDir: dir, SecretsKey: testKey}, WithDocker(w.fake), WithVIP(vipStub{}),
		WithProxy(func(_ context.Context, cfg json.RawMessage) error {
			w.mu.Lock()
			w.pushed = append(w.pushed, string(cfg))
			w.mu.Unlock()
			return nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	w.o, w.st = o, storetest.Open(t, filepath.Join(dir, "stackr.db"))
	ctx := context.Background()
	now := time.Now()
	w.org, w.stack, w.env = uuid.NewString(), uuid.NewString(), uuid.NewString()
	must(t, w.st.Orgs.Create(ctx, store.Org{ID: w.org, Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, w.st.Stacks.Create(ctx, store.Stack{ID: w.stack, OrgID: w.org, Name: "shop", Slug: "shop", Settings: "{}", Domains: "[]", CreatedAt: now}))
	must(t, w.st.Environments.Create(ctx, store.Environment{ID: w.env, StackID: w.stack, Name: "dev", Slug: "dev", Type: "static",
		Settings: "{}", Network: "n", FromKind: "branch", FromBranch: "main", CreatedAt: now}))
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
	tl, err := w.o.CreateTile(context.Background(), Tile{StackID: w.stack, EnvironmentID: w.env, Name: name,
		Kind: tile.Image, ImageRef: "nginx:1", ContainerPort: 80})
	must(t, err)
	if running {
		w.fake.Containers = append(w.fake.Containers, docker.Container{ID: "c-" + name, Name: "c-" + name, State: "running",
			Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"}})
	}
	return tl
}

// wait polls a job until it finishes.
func (w *world) wait(t *testing.T, id string) Job {
	t.Helper()
	for range 500 {
		j, err := w.o.GetJob(context.Background(), id)
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
	must(t, w.o.SetParams(ctx, ParamScope{Kind: "stack", ID: w.stack},
		[]ParamEntry{{Collection: "app", Name: "mode", Kind: "param", Value: "fast"}}))
	js, err := w.o.TileJobs(ctx, []string{up.ID, down.ID}, 10)
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
	must(t, w.st.Stacks.Create(ctx, store.Stack{ID: other, OrgID: w.org, Name: "blog", Slug: "blog", Settings: "{}", Domains: "[]", CreatedAt: time.Now()}))
	rel, err := w.o.releases.Create(ctx, other, "test", nil)
	must(t, err)

	pp, err := w.o.PlanPromote(ctx, w.env, rel.ID)
	must(t, err)
	if pp.CanDeploy || len(pp.Plan.Blockers) != 1 {
		t.Fatalf("plan = %+v, want one blocker", pp)
	}
	for _, verb := range []func(context.Context, string, string) (Job, error){w.o.Promote, w.o.Rollback} {
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
	_, err := w.o.AttachDomain(ctx, api.ID, DomainSpec{Host: "api.example.com"})
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
	got, _, err := w.o.UpdateTile(ctx, api.ID, func(t *Tile) error { t.HealthPath = "/up"; return nil })
	must(t, err)
	if got.HealthPath != "/up" || got.ImageRef != "nginx:1" || got.ContainerPort != 80 {
		t.Errorf("after update: %+v", got)
	}
}
