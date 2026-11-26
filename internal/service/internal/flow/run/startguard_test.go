package run_test

import (
	"errors"
	"io"
	"testing"

	lrun "github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/startguard"
)

// A manual run whose guard fails after the spec is made never starts its
// container, and the run says failed.
func TestRunGuardStopsBeforeTheContainer(t *testing.T) {
	f, fake, sweep := setup(t)
	r := queue(t, f, sweep.ID)
	gctx := startguard.With(ctx, func() error { return errors.New("the disk failed meanwhile") })
	_ = f.Do(gctx, r.ID, io.Discard)
	r, _ = f.Runs.Get(ctx, sweep.ID, r.ID)
	if r.Status != lrun.Failed {
		t.Errorf("run = %+v, want failed", r)
	}
	if len(fake.Specs) != 0 {
		t.Errorf("a container was made: %+v", fake.Specs)
	}
}
