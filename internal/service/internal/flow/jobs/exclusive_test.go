package jobs_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
)

// DL: simultaneous exclusive enqueues on one key: exactly one wins, the rest
// are refused and never supersede it.
func TestEnqueueExclusiveRefusesInsteadOfSuperseding(t *testing.T) {
	g := newGate(false)
	r, l := setup(t, map[jobs.Kind]jobs.Handler{"k": g.run}, jobs.Options{})
	start(t, r)
	busy := errs.Conflictf("busy")
	var wg sync.WaitGroup
	var won atomic.Int32
	var ids sync.Map
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := r.EnqueueExclusive(ctx, "k", job.LockSet("a", "b"), "{}", nil, []string{"a"}, busy)
			switch {
			case err == nil:
				won.Add(1)
				ids.Store(j.ID, true)
			case !errors.Is(err, busy):
				t.Errorf("err = %v", err)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d presses won, want 1", won.Load())
	}
	id := started(t, g)
	if _, ok := ids.Load(id); !ok {
		t.Fatalf("job %s ran, not the winner", id)
	}
	close(g.release)
	waitState(t, l, id, job.Done)
}
