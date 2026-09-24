package jobs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

// gate is a handler that reports it started and then blocks until released
// or cancelled; swap makes it enter the swap phase first.
type gate struct {
	started chan string
	release chan struct{}
	swap    bool
}

func newGate(swap bool) *gate {
	return &gate{started: make(chan string, 10), release: make(chan struct{}), swap: swap}
}

func (g *gate) run(ctx context.Context, r *jobs.Run) error {
	if g.swap {
		if err := r.Swap(); err != nil {
			return err
		}
	}
	g.started <- r.Job.ID
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func setup(t *testing.T, h map[jobs.Kind]jobs.Handler, opt jobs.Options) (*jobs.Runner, *job.Leaf) {
	t.Helper()
	st := servicetest.Store(t)
	l := job.New(st.Jobs)
	if opt.Poll == 0 {
		opt.Poll = 10 * time.Millisecond
	}
	if opt.Workers == nil {
		opt.Workers = func(context.Context) (int, error) { return 4, nil }
	}
	r := jobs.New(l, h, t.TempDir(), opt)
	return r, l
}

func start(t *testing.T, r *jobs.Runner) {
	t.Helper()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
}

func enqueue(t *testing.T, r *jobs.Runner, kind jobs.Kind, tiles ...string) string {
	t.Helper()
	j, err := r.Enqueue(ctx, kind, job.LockSet(tiles...), "{}", nil)
	if err != nil {
		t.Fatal(err)
	}
	return j.ID
}

// waitState polls the row until it reaches want.
func waitState(t *testing.T, l *job.Leaf, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, err := l.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s: state %q (error %q), want %q", id, j.State, j.Error, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func started(t *testing.T, g *gate) string {
	t.Helper()
	select {
	case id := <-g.started:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
		return ""
	}
}

func notStarted(t *testing.T, g *gate) {
	t.Helper()
	select {
	case id := <-g.started:
		t.Fatalf("job %s started, want it held", id)
	case <-time.After(100 * time.Millisecond):
	}
}

// Two jobs on the same tile run one after the other; a job on another tile
// runs beside them.
func TestLockSet(t *testing.T) {
	a, b := newGate(false), newGate(false)
	r, l := setup(t, map[jobs.Kind]jobs.Handler{"a": a.run, "b": b.run}, jobs.Options{})
	start(t, r)

	first := enqueue(t, r, "a", "t1")
	if got := started(t, a); got != first {
		t.Fatalf("started %s, want %s", got, first)
	}
	second := enqueue(t, r, "b", "t1", "t2") // different kind: queues, never supersedes
	notStarted(t, b)
	waitState(t, l, second, job.Queued)

	// A job behind the blocked one on t2 must not overtake it.
	third := enqueue(t, r, "a", "t2")
	notStarted(t, a)

	other := enqueue(t, r, "a", "t9")
	if got := started(t, a); got != other {
		t.Fatalf("disjoint job: started %s, want %s", got, other)
	}

	a.release <- struct{}{} // first or other; either frees a slot
	a.release <- struct{}{}
	waitState(t, l, first, job.Done)
	if got := started(t, b); got != second {
		t.Fatalf("started %s, want %s", got, second)
	}
	close(b.release)
	waitState(t, l, second, job.Done)
	started(t, a)
	close(a.release)
	waitState(t, l, third, job.Done)
}

func TestWorkersCap(t *testing.T) {
	g := newGate(false)
	r, l := setup(t, map[jobs.Kind]jobs.Handler{"a": g.run},
		jobs.Options{Workers: func(context.Context) (int, error) { return 1, nil }})
	start(t, r)
	one := enqueue(t, r, "a", "t1")
	started(t, g)
	two := enqueue(t, r, "a", "t2")
	notStarted(t, g)
	g.release <- struct{}{}
	waitState(t, l, one, job.Done)
	started(t, g)
	close(g.release)
	waitState(t, l, two, job.Done)
}

// The Railway rule: a newer job of the same kind covering the tiles
// supersedes older queued and building ones, and waits for a swapping one.
func TestSupersede(t *testing.T) {
	t.Run("queued", func(t *testing.T) {
		g := newGate(false)
		r, l := setup(t, map[jobs.Kind]jobs.Handler{"a": g.run}, jobs.Options{})
		old := enqueue(t, r, "a", "t1") // not started yet: the runner is off
		newer := enqueue(t, r, "a", "t1", "t2")
		waitState(t, l, old, job.Superseded)
		start(t, r)
		if got := started(t, g); got != newer {
			t.Fatalf("started %s, want %s", got, newer)
		}
		close(g.release)
		waitState(t, l, newer, job.Done)
	})
	t.Run("building", func(t *testing.T) {
		g := newGate(false)
		r, l := setup(t, map[jobs.Kind]jobs.Handler{"a": g.run}, jobs.Options{})
		start(t, r)
		old := enqueue(t, r, "a", "t1")
		started(t, g)
		newer := enqueue(t, r, "a", "t1")
		waitState(t, l, old, job.Superseded)
		started(t, g)
		close(g.release)
		waitState(t, l, newer, job.Done)
	})
	t.Run("swapping", func(t *testing.T) {
		g := newGate(true)
		r, l := setup(t, map[jobs.Kind]jobs.Handler{"a": g.run}, jobs.Options{})
		start(t, r)
		old := enqueue(t, r, "a", "t1")
		started(t, g)
		newer := enqueue(t, r, "a", "t1")
		notStarted(t, g)
		waitState(t, l, old, job.Running)
		g.release <- struct{}{}
		waitState(t, l, old, job.Done)
		started(t, g)
		close(g.release)
		waitState(t, l, newer, job.Done)
	})
	t.Run("smaller lock set queues", func(t *testing.T) {
		g := newGate(false)
		r, l := setup(t, map[jobs.Kind]jobs.Handler{"a": g.run}, jobs.Options{})
		old := enqueue(t, r, "a", "t1", "t2")
		enqueue(t, r, "a", "t1")
		waitState(t, l, old, job.Queued)
	})
}

func TestCancel(t *testing.T) {
	g, s := newGate(false), newGate(true)
	r, l := setup(t, map[jobs.Kind]jobs.Handler{"a": g.run, "s": s.run}, jobs.Options{})
	start(t, r)

	building := enqueue(t, r, "a", "t1")
	started(t, g)
	queued := enqueue(t, r, "s", "t1")
	if err := r.Cancel(ctx, queued); err != nil {
		t.Fatal(err)
	}
	waitState(t, l, queued, job.Cancelled)
	if err := r.Cancel(ctx, building); err != nil {
		t.Fatal(err)
	}
	waitState(t, l, building, job.Cancelled)
	if err := r.Cancel(ctx, building); err != nil {
		t.Fatalf("cancelling a finished job: %v, want nil", err)
	}

	swapping := enqueue(t, r, "s", "t2")
	started(t, s)
	if _, ok := errs.IsConflict(r.Cancel(ctx, swapping)); !ok {
		t.Fatal("cancelling mid-swap: want a Conflict")
	}
	close(s.release)
	waitState(t, l, swapping, job.Done)
}

// A job that needs an unset param parks and comes back once it is set.
func TestWaiting(t *testing.T) {
	set := make(chan bool, 1)
	set <- false
	calls := 0
	h := func(ctx context.Context, r *jobs.Run) error {
		calls++
		if calls == 1 {
			return errs.Unset{Param: "DB_URL"}
		}
		return nil
	}
	r, l := setup(t, map[jobs.Kind]jobs.Handler{"a": h}, jobs.Options{
		ParamSet: func(_ context.Context, p string) (bool, error) {
			if p != "DB_URL" {
				return false, errors.New("wrong param " + p)
			}
			select {
			case v := <-set:
				return v, nil
			default:
				return false, nil
			}
		},
	})
	start(t, r)
	id := enqueue(t, r, "a", "t1")
	waitState(t, l, id, job.Waiting)
	j, _ := l.Get(ctx, id)
	if j.WaitingParam == nil || *j.WaitingParam != "DB_URL" {
		t.Fatalf("waiting_param = %v", j.WaitingParam)
	}
	set <- true
	waitState(t, l, id, job.Done)
}

func TestFailures(t *testing.T) {
	block := func(ctx context.Context, _ *jobs.Run) error { <-ctx.Done(); return ctx.Err() }
	r, l := setup(t, map[jobs.Kind]jobs.Handler{
		"slow":  block,
		"boom":  func(context.Context, *jobs.Run) error { panic("kaboom") },
		"error": func(context.Context, *jobs.Run) error { return errors.New("pull failed") },
	}, jobs.Options{Cap: 50 * time.Millisecond})
	start(t, r)
	for _, c := range []struct {
		kind jobs.Kind
		want string
	}{
		{"slow", "cap"},
		{"boom", "panic: kaboom"},
		{"error", "pull failed"},
		{"nope", "no handler"},
	} {
		id := enqueue(t, r, c.kind, "t-"+string(c.kind))
		waitState(t, l, id, job.Failed)
		j, _ := l.Get(ctx, id)
		if !strings.Contains(j.Error, c.want) {
			t.Errorf("%s: error %q, want it to contain %q", c.kind, j.Error, c.want)
		}
	}
}

// An uncapped kind outlives the cap; its handler holds its own clock.
func TestUncapped(t *testing.T) {
	r, l := setup(t, map[jobs.Kind]jobs.Handler{
		"run": func(ctx context.Context, _ *jobs.Run) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(150 * time.Millisecond):
				return nil
			}
		},
	}, jobs.Options{Cap: 50 * time.Millisecond, Uncapped: map[jobs.Kind]bool{"run": true}})
	start(t, r)
	id := enqueue(t, r, "run", "t1")
	waitState(t, l, id, job.Done)
}

// A row left running by a dead process is failed at boot, not resumed.
func TestRestartRecovery(t *testing.T) {
	r, l := setup(t, nil, jobs.Options{})
	j, err := l.Create(ctx, "a", []string{"t1"}, "{}", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Start(ctx, j); err != nil {
		t.Fatal(err)
	}
	start(t, r)
	waitState(t, l, j.ID, job.Failed)
	got, _ := l.Get(ctx, j.ID)
	if !strings.Contains(got.Error, "restarted") {
		t.Fatalf("error = %q", got.Error)
	}
}
