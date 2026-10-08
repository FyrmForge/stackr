package deploy_test

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// depWorld is setup plus a dependency tile "db" and api depending on dep.
// The sleeps are counted, never waited.
type depWorld struct {
	*world
	t      *testing.T
	db     store.Tile
	slept  time.Duration
	onWake func(n int)
	n      int
}

func setupDep(t *testing.T, dep string, db store.Tile) *depWorld {
	w := setup(t)
	db.StackID, db.EnvironmentID, db.Name = w.tile.StackID, w.env.ID, "db"
	var err error
	db, err = w.f.Tiles.Create(ctx, db)
	must(t, err)
	api := w.tile
	cur := api
	cur.DependsOn = dep
	api, _, err = w.f.Tiles.Update(ctx, api, cur)
	must(t, err)
	w.tile = api
	w.f.Runs = run.New(w.st.Runs, t.TempDir())
	d := &depWorld{world: w, t: t, db: db}
	w.f.DepSleep = func(_ context.Context, dur time.Duration) error {
		d.slept += dur
		d.n++
		if d.onWake != nil {
			d.onWake(d.n)
		}
		return nil
	}
	return d
}

func (d *depWorld) dbState(state, health string) {
	d.fake.Containers = slices.DeleteFunc(d.fake.Containers, func(c docker.Container) bool {
		return c.Labels[tile.LabelTile] == d.db.ID
	})
	d.fake.Containers = append(d.fake.Containers, docker.Container{
		ID: "dbc", State: state, Health: health,
		Labels: map[string]string{tile.LabelTile: d.db.ID, tile.LabelRole: "replica"},
	})
}

func deployAPI(d *depWorld) error {
	_, err := d.f.Run(ctx, d.tile, "nginx@sha256:aa", io.Discard, nil)
	return err
}

func TestHealthyWaitPasses(t *testing.T) {
	d := setupDep(t, "db:healthy", store.Tile{Kind: tile.Image, ImageRef: "pg:1", ContainerPort: 5432, HealthcheckIntervalS: 2, HealthcheckRetries: 3})
	d.dbState("running", "starting")
	d.onWake = func(n int) {
		if n == 2 {
			d.dbState("running", "healthy")
		}
	}
	if err := deployAPI(d); err != nil {
		t.Fatal(err)
	}
	if d.n != 2 {
		t.Errorf("slept %d times, want 2", d.n)
	}
}

func TestHealthyWaitTimesOut(t *testing.T) {
	d := setupDep(t, "db:healthy", store.Tile{Kind: tile.Image, ImageRef: "pg:1", ContainerPort: 5432,
		HealthcheckIntervalS: 5, HealthcheckRetries: 4, HealthcheckStartPeriodS: 10})
	d.dbState("running", "starting")
	err := deployAPI(d)
	if err == nil || !strings.Contains(err.Error(), "waited for db to be healthy; it is not") {
		t.Fatalf("err = %v", err)
	}
	if d.slept < 30*time.Second || d.slept > 32*time.Second {
		t.Errorf("waited %v, want start period + retries*interval = 30s", d.slept)
	}
	if got := d.fake.Calls(); at(got, "Run", "") >= 0 {
		t.Errorf("api must not start: %v", got)
	}
}

func TestHealthyWaitCapped(t *testing.T) {
	d := setupDep(t, "db:healthy", store.Tile{Kind: tile.Image, ImageRef: "pg:1", ContainerPort: 5432,
		HealthcheckIntervalS: 600, HealthcheckRetries: 9})
	d.dbState("running", "starting")
	if err := deployAPI(d); err == nil {
		t.Fatal("want timeout")
	}
	if d.slept > 10*time.Minute+5*time.Second {
		t.Errorf("waited %v, want at most 10m", d.slept)
	}
}

func TestStartedDepDoesNotWait(t *testing.T) {
	d := setupDep(t, "db", store.Tile{Kind: tile.Image, ImageRef: "pg:1", ContainerPort: 5432})
	d.dbState("running", "starting")
	if err := deployAPI(d); err != nil || d.n != 0 {
		t.Fatalf("err = %v, sleeps = %d", err, d.n)
	}
}

func TestWaitStopsOnCancel(t *testing.T) {
	d := setupDep(t, "db:healthy", store.Tile{Kind: tile.Image, ImageRef: "pg:1", ContainerPort: 5432})
	d.dbState("running", "starting")
	c, cancel := context.WithCancel(ctx)
	d.f.DepSleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	if _, err := d.f.Run(c, d.tile, "nginx@sha256:aa", io.Discard, nil); err == nil {
		t.Fatal("want the wait cancelled")
	}
}

func fn(trigger string) store.Tile {
	return store.Tile{Kind: tile.Function, ImageRef: "busybox:1", Trigger: trigger}
}

func TestCompletedWaitsForAnActiveRun(t *testing.T) {
	d := setupDep(t, "db:completed", fn(tile.Manual))
	r, err := d.f.Runs.Start(ctx, d.db.ID, nil, run.Manual)
	must(t, err)
	d.onWake = func(n int) {
		code := 0
		_, err := d.f.Runs.Finish(ctx, r.ID, &code, run.OK, "")
		must(t, err)
	}
	if err := deployAPI(d); err != nil || d.n != 1 {
		t.Fatalf("err = %v, sleeps = %d", err, d.n)
	}
}

func TestCompletedPassesOnLastRunOK(t *testing.T) {
	d := setupDep(t, "db:completed", fn(tile.Manual))
	r, err := d.f.Runs.Start(ctx, d.db.ID, nil, run.Manual)
	must(t, err)
	code := 0
	_, err = d.f.Runs.Finish(ctx, r.ID, &code, run.OK, "")
	must(t, err)
	if err := deployAPI(d); err != nil || d.n != 0 {
		t.Fatalf("err = %v, sleeps = %d", err, d.n)
	}
}

func TestCompletedFailsOnFailedRun(t *testing.T) {
	d := setupDep(t, "db:completed", fn(tile.Manual))
	r, err := d.f.Runs.Start(ctx, d.db.ID, nil, run.Manual)
	must(t, err)
	code := 1
	_, err = d.f.Runs.Finish(ctx, r.ID, &code, run.Failed, "exit 1")
	must(t, err)
	err = deployAPI(d)
	if err == nil || !strings.Contains(err.Error(), "waited for db to complete; it did not") {
		t.Fatalf("err = %v", err)
	}
}

func TestCompletedRunsAnOnDeployFunctionFirst(t *testing.T) {
	d := setupDep(t, "db:completed", fn(tile.OnDeploy))
	var ran []string
	d.f.RunFirst = func(_ context.Context, id string, _ io.Writer) error {
		ran = append(ran, id)
		return nil
	}
	if err := deployAPI(d); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0] != d.db.ID {
		t.Fatalf("ran = %v", ran)
	}
}

// A dependency with no replica containers (a slice, a function, never
// deployed) cannot become healthy by waiting: fail at once.
func TestHealthyNothingRunningFailsAtOnce(t *testing.T) {
	d := setupDep(t, "db:healthy", store.Tile{Kind: tile.Image, ImageRef: "pg:1", ContainerPort: 5432})
	d.fake.Containers = slices.DeleteFunc(d.fake.Containers, func(c docker.Container) bool {
		return c.Labels[tile.LabelTile] == d.db.ID
	})
	err := deployAPI(d)
	if err == nil || !strings.Contains(err.Error(), "db has nothing running") {
		t.Fatalf("err = %v", err)
	}
	if d.n != 0 {
		t.Errorf("slept %d times, want a failure at once", d.n)
	}
}

// An exited leftover next to a healthy replica does not fail the check.
func TestHealthyIgnoresExitedLeftover(t *testing.T) {
	d := setupDep(t, "db:healthy", store.Tile{Kind: tile.Image, ImageRef: "pg:1", ContainerPort: 5432})
	d.dbState("running", "healthy")
	d.fake.Containers = append(d.fake.Containers, docker.Container{
		ID: "dbold", State: "exited",
		Labels: map[string]string{tile.LabelTile: d.db.ID, tile.LabelRole: "replica"},
	})
	if err := deployAPI(d); err != nil || d.n != 0 {
		t.Fatalf("err = %v, sleeps = %d", err, d.n)
	}
}

// An explicit retries of 1 or 2 is kept: 1 retry at 5s is 5s, not 15s.
func TestHealthyExplicitRetriesKept(t *testing.T) {
	d := setupDep(t, "db:healthy", store.Tile{Kind: tile.Image, ImageRef: "pg:1", ContainerPort: 5432,
		HealthcheckIntervalS: 5, HealthcheckRetries: 1})
	d.dbState("running", "starting")
	if err := deployAPI(d); err == nil {
		t.Fatal("want a timeout")
	}
	if d.slept < 5*time.Second || d.slept > 7*time.Second {
		t.Errorf("waited %v, want retries*interval = 5s", d.slept)
	}
}

// A tile that sets no healthcheck times out by its image's own HEALTHCHECK.
func TestHealthyReadsTheImageHealthcheck(t *testing.T) {
	d := setupDep(t, "db:healthy", store.Tile{Kind: tile.Image, ImageRef: "pg:1", ContainerPort: 5432})
	d.dbState("running", "starting")
	d.fake.Details["dbc"] = docker.Detail{
		Running: true, HealthInterval: 4 * time.Second, HealthRetries: 2, HealthStartPeriod: 6 * time.Second,
	}
	if err := deployAPI(d); err == nil {
		t.Fatal("want a timeout")
	}
	if d.slept < 14*time.Second || d.slept > 16*time.Second {
		t.Errorf("waited %v, want start 6s + 2 retries * 4s = 14s", d.slept)
	}
}

// queueJob is a queued job touching the dependency, as one waiting for the
// deploy's worker or lock would be.
func (d *depWorld) queueJob(kind string) {
	_, err := d.f.Jobs.Create(ctx, kind, []string{d.db.ID}, "", nil, d.t.TempDir())
	must(d.t, err)
}

// A deploy of the dependency queued behind this one cannot finish while
// this one waits for it: fail at once.
func TestHealthyQueuedDeployFailsAtOnce(t *testing.T) {
	d := setupDep(t, "db:healthy", store.Tile{Kind: tile.Image, ImageRef: "pg:1", ContainerPort: 5432})
	d.dbState("running", "starting")
	d.queueJob("deploy")
	err := deployAPI(d)
	if err == nil || !strings.Contains(err.Error(), "db has a deploy queued behind this deploy") {
		t.Fatalf("err = %v", err)
	}
	if d.n != 0 {
		t.Errorf("slept %d times, want a failure at once", d.n)
	}
}

// A run of the function queued behind the deploy's own lock never starts
// while the deploy waits for it: fail at once, not after ten minutes.
func TestCompletedQueuedRunFailsAtOnce(t *testing.T) {
	d := setupDep(t, "db:completed", fn(tile.Manual))
	_, err := d.f.Runs.Start(ctx, d.db.ID, nil, run.Manual)
	must(t, err)
	d.queueJob("run")
	err = deployAPI(d)
	if err == nil || !strings.Contains(err.Error(), "db has a run queued behind this deploy; let it finish and promote again") {
		t.Fatalf("err = %v", err)
	}
	if d.n != 0 {
		t.Errorf("slept %d times, want a failure at once", d.n)
	}
}
