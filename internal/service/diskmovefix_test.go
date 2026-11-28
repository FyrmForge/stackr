package service

import (
	"os"

	"context"
	"errors"
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
)

// The panel move job holds todiskKey itself, so its own plan must not count
// that as volumes moving to the disk.
func TestPanelMovePlanForJobIgnoresItsOwnTodiskLock(t *testing.T) {
	ctx := context.Background()
	r := newObserveRig(t)
	r.orch.cfg.PanelSpec = func(string) ContainerSpec { return ContainerSpec{} }
	r.orch.cfg.Version = "v1"
	r.row.Kind = disk.KindXFS
	must(t, r.st.NodeDisks.Update(ctx, r.row))
	if _, err := r.orch.enqueue(ctx, kindObserveMove, observeMoveJob{NodeID: r.ctl()}, todiskKey(r.ctl())); err != nil {
		t.Fatal(err)
	}
	const todisk = "Volumes are moving to the disk. Wait for them."
	if p, err := r.orch.planWith(ctx, r.ctl(), false, false); err != nil || p.Refusal != todisk {
		t.Fatalf("plan with a rival = %q, %v (the rig must reach the todisk check)", p.Refusal, err)
	}
	if p, _ := r.orch.planForJob(ctx, movePanelJob{NodeID: r.ctl()}); p.Refusal == todisk {
		t.Errorf("the job refused itself: %q", p.Refusal)
	}
}

// A disk that cannot keep observe_min_free after the copy is refused up front.
func TestObserveMovePlanRefusesADiskUnderMinFree(t *testing.T) {
	ctx := context.Background()
	r := newObserveRig(t)
	r.ctlFacts(t, ctlDiskFacts(3<<30))
	_, err := r.orch.PlanMoveObserve(ctx, adminP(), r.ctl(), false)
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "free after the move") {
		t.Fatalf("3 GB disk: %v", err)
	}
	r.ctlFacts(t, ctlDiskFacts(50<<30))
	if _, err := r.orch.PlanMoveObserve(ctx, adminP(), r.ctl(), false); err != nil {
		t.Errorf("50 GB disk: %v", err)
	}
}

// A move that fails before the switch removes its half copy and folder.
func TestObserveMoveFailBeforeSwitchRemovesTheHalfCopy(t *testing.T) {
	r := newObserveRig(t)
	r.ctlFake.OnUntar = nil // the copy lands empty, which the check refuses
	j := r.move(t, false)
	if j.State != job.Failed || !strings.Contains(j.Error, "missing or empty") {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	if _, err := os.Stat(r.folder(r.row, installspec.ObserveDiskVolume)); !os.IsNotExist(err) {
		t.Errorf("the half copy's folder stayed: %v", err)
	}
	if r.removed(installspec.ObserveVolume) {
		t.Error("the original volume was removed")
	}
}
