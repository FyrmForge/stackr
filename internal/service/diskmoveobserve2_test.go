package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/upgrade"
)

// The job holds the target disk's lock, so a Forget or Adopt (which lock
// disk:<id>) cannot overlap it.
func TestObserveMoveTakesTheDiskLock(t *testing.T) {
	r := newObserveRig(t)
	release := make(chan struct{})
	r.orch.observe.check = func(context.Context) error { <-release; return nil }
	j, err := r.orch.MoveObserve(context.Background(), adminP(), r.ctl(), false)
	must(t, err)
	defer close(release)
	if !slices.Contains(j.LockSet, "disk:"+r.row.ID) || !slices.Contains(j.LockSet, todiskKey(r.ctl())) {
		t.Errorf("lock set = %v", j.LockSet)
	}
}

// An unreadable size is a refusal, not an empty copy that always fits.
func TestObserveMoveRefusesUnknownSize(t *testing.T) {
	r := newObserveRig(t)
	r.ctlFake.Volumes = nil
	if _, err := r.orch.PlanMoveObserve(context.Background(), adminP(), r.ctl(), false); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("unknown size: %v", err)
	}
}

// The disk going missing (or coming back) restarts the guard at once.
func TestObserveResyncOnDiskChange(t *testing.T) {
	ctx := context.Background()
	r := newObserveRig(t)
	must(t, r.st.Settings.Set(ctx, observeDataDiskKey, "1"))
	setState(t, r.diskTest, r.row, "missing")
	r.orch.resyncObserve(ctx, r.ctl())
	for range 200 {
		if e := r.orch.observe.lastErr.Load(); e != nil && *e == upgrade.ObserveDiskMissing {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("observe was not stopped when the disk went missing")
}
