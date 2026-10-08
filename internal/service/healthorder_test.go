package service

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	lrun "github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// onDeployFn is an on_deploy function in the world's dev env; env, when set,
// is its env json.
func (w *world) onDeployFn(t *testing.T, env string) Tile {
	t.Helper()
	if env == "" {
		env = "{}"
	}
	fn, err := w.orch.CreateTile(context.Background(), Tile{
		StackID:       w.stack,
		EnvironmentID: w.env,
		Name:          "migrate",
		Kind:          tile.Function,
		ImageRef:      "busybox:1",
		Trigger:       tile.OnDeploy,
		EnvJSON:       env,
	})
	must(t, err)
	return fn
}

func (w *world) runCount(t *testing.T, id string) int {
	t.Helper()
	rs, err := w.orch.Runs(context.Background(), id, 0)
	must(t, err)
	return len(rs)
}

// A dependent whose function parks on an unset param must not leave the
// function's run queued: it would block every later run of it.
func TestRunFirstUnsetLeavesNoQueuedRow(t *testing.T) {
	w := newWorld(t)
	ctx := withRanFirst(context.Background(), time.Time{})
	fn := w.onDeployFn(t, `{"T":"${{ params.app.token }}"}`)
	err := w.orch.runFirst(ctx, fn.ID, io.Discard)
	if _, ok := errs.IsUnset(err); !ok {
		t.Fatalf("err = %v, want parked on the unset param", err)
	}
	if a, active, _ := w.orch.runs.Active(ctx, fn.ID); active {
		t.Fatalf("run %s is still %s: nothing will ever close it", a.ID, a.Status)
	}
}

// A queued run no job carries (a restart between Queue and Begin) is closed
// at boot like a running one.
func TestInterruptedClosesQueuedRunWithoutJob(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	fn := w.onDeployFn(t, "")
	_, err := w.orch.runs.Start(ctx, fn.ID, nil, lrun.Deploy)
	must(t, err)
	must(t, w.orch.runs.Interrupted(ctx))
	if a, active, _ := w.orch.runs.Active(ctx, fn.ID); active {
		t.Fatalf("run %s is still %s after a restart", a.ID, a.Status)
	}
}

// Two dependents on one function run it once in a deploy.
func TestRunFirstOnceForTwoDependents(t *testing.T) {
	w := newWorld(t)
	ctx := withRanFirst(context.Background(), time.Time{})
	fn := w.onDeployFn(t, "")
	must(t, w.orch.runFirst(ctx, fn.ID, io.Discard))
	must(t, w.orch.runFirst(ctx, fn.ID, io.Discard))
	if n := w.runCount(t, fn.ID); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

// A resumed job has lost its in-memory marker: the run it already did since
// it was created counts.
func TestRunFirstResumeDoesNotRerun(t *testing.T) {
	w := newWorld(t)
	since := time.Now().Add(-time.Minute)
	fn := w.onDeployFn(t, "")
	must(t, w.orch.runFirst(withRanFirst(context.Background(), since), fn.ID, io.Discard))
	must(t, w.orch.runFirst(withRanFirst(context.Background(), since), fn.ID, io.Discard))
	if n := w.runCount(t, fn.ID); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}

// A run of the function already going is waited for, not a conflict.
func TestRunFirstWaitsForAnActiveRun(t *testing.T) {
	w := newWorld(t)
	ctx := withRanFirst(context.Background(), time.Time{})
	fn := w.onDeployFn(t, "")
	going, err := w.orch.runs.Start(ctx, fn.ID, nil, lrun.Manual)
	must(t, err)
	slept := 0
	w.orch.deploy.DepSleep = func(context.Context, time.Duration) error {
		slept++
		code := 0
		_, err := w.orch.runs.Finish(ctx, going.ID, &code, lrun.OK, "")
		return err
	}
	if err := w.orch.runFirst(ctx, fn.ID, io.Discard); err != nil {
		t.Fatalf("err = %v, want it to wait and then run", err)
	}
	if slept != 1 || w.runCount(t, fn.ID) != 2 {
		t.Fatalf("slept %d, runs %d; want one wait and its own run", slept, w.runCount(t, fn.ID))
	}
}
