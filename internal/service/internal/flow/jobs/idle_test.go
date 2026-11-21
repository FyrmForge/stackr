package jobs_test

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
)

// A job that waits idle gives its worker to the job it waits for: with one
// worker, the waiter would otherwise starve it for the whole cap.
func TestIdleFreesTheWorker(t *testing.T) {
	g := newGate(false)
	waiting := make(chan struct{})
	r, l := setup(t, map[jobs.Kind]jobs.Handler{
		"wait": func(ctx context.Context, _ *jobs.Run) error {
			defer jobs.Idle(ctx)()
			close(waiting)
			select { // until the other job has run
			case <-g.started:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		"a": g.run,
	}, jobs.Options{Workers: func(context.Context) (int, error) { return 1, nil }})
	start(t, r)
	w := enqueue(t, r, "wait", "t1")
	<-waiting
	a := enqueue(t, r, "a", "t2")
	waitState(t, l, w, job.Done)
	close(g.release)
	waitState(t, l, a, job.Done)
}

func TestIdleOutsideAJobIsANoop(t *testing.T) {
	jobs.Idle(context.Background())()
}

// Unpooled kinds start with the pool full; their lock sets still order them.
func TestUnpooledIgnoresWorkers(t *testing.T) {
	g := newGate(false)
	u := newGate(false)
	r, l := setup(t, map[jobs.Kind]jobs.Handler{"pool": g.run, "u": u.run},
		jobs.Options{
			Workers:  func(context.Context) (int, error) { return 1, nil },
			Unpooled: map[jobs.Kind]bool{"u": true},
		})
	start(t, r)
	enqueue(t, r, "pool", "p")
	started(t, g)
	enqueue(t, r, "u", "t1")
	enqueue(t, r, "u", "t2")
	started(t, u)
	started(t, u)
	close(u.release)

	// Same tile: one after another.
	o := newGate(false)
	r2, l2 := setup(t, map[jobs.Kind]jobs.Handler{"u": o.run, "v": o.run},
		jobs.Options{
			Workers:  func(context.Context) (int, error) { return 1, nil },
			Unpooled: map[jobs.Kind]bool{"u": true, "v": true},
		})
	start(t, r2)
	a := enqueue(t, r2, "u", "env")
	b := enqueue(t, r2, "v", "env")
	started(t, o)
	notStarted(t, o)
	close(o.release)
	waitState(t, l2, a, job.Done)
	waitState(t, l2, b, job.Done)
	close(g.release)
	_ = l
}
