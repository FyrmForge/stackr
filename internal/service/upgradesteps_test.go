package service

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/panel"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A rolled-back upgrade keeps the new panel's exit code and log: the helper's
// file is read into settings at boot and gone after, and a new upgrade clears it.
func TestUndoKeepsTheNewPanelsLog(t *testing.T) {
	r := newRollbackRig(t, 1)
	ctx := context.Background()
	db := r.n.orch.cfg.DBPath
	b, _ := json.Marshal(panel.Failure{Tag: "0.9.1", Reason: "the container exited during the health check", ExitCode: 3, Log: "panic: no such column"})
	must(t, os.WriteFile(panel.FailureFile(db), b, 0o600))
	t.Cleanup(func() { _ = os.Remove(panel.FailureFile(db)) })

	r.n.orch.undoFailedUpgrade(ctx)

	f, ok := r.n.orch.LastUpgradeFailure(ctx)
	if !ok || f.Tag != "0.9.1" || f.ExitCode != 3 || f.Log != "panic: no such column" {
		t.Fatalf("failure = %+v, %v", f, ok)
	}
	if _, err := os.Stat(panel.FailureFile(db)); err == nil {
		t.Error("the helper's file is left behind")
	}
}

// A failure file left by another trial (another target tag) is not kept
// as this one's reason.
func TestUndoIgnoresAnotherTrialsFile(t *testing.T) {
	r := newRollbackRig(t, 1)
	ctx := context.Background()
	db := r.n.orch.cfg.DBPath
	b, _ := json.Marshal(panel.Failure{Tag: "0.8.0", Reason: "old", Log: "old log"})
	must(t, os.WriteFile(panel.FailureFile(db), b, 0o600))
	t.Cleanup(func() { _ = os.Remove(panel.FailureFile(db)) })

	r.n.orch.undoFailedUpgrade(ctx)

	if f, ok := r.n.orch.LastUpgradeFailure(ctx); ok {
		t.Fatalf("kept another trial's failure: %+v", f)
	}
}

// The tab reads the step the flow reported, and after the swap says every
// step is done once this build is the target.
func TestUpgradeProgress(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	o := n.orch
	if p := o.UpgradeProgress(ctx); p.Step != 0 {
		t.Fatalf("idle step = %d", p.Step)
	}
	start := time.Now().Add(-21 * time.Second)
	j := store.Job{ID: uuid.NewString(), Kind: string(kindUpgrade), State: "running", Payload: `{"tag":"v0.9.1"}`, LockSet: store.StringList{}, CreatedAt: start, StartedAt: &start}
	must(t, n.st.Jobs.Create(ctx, j))

	o.setUpgradeStep("archive")
	p := o.UpgradeProgress(ctx)
	if p.Step != UpgradeArchive || p.Target != "v0.9.1" || p.Elapsed < 21*time.Second {
		t.Errorf("progress = %+v", p)
	}
	o.upgradeStep.Store(0)

	o.upgrading.Store(true) // the helper runs, this panel is not the target
	o.cfg.Version = "v0.9.0"
	if p := o.UpgradeProgress(ctx); p.Step != UpgradeCheck {
		t.Errorf("trial step = %d", p.Step)
	}
	o.cfg.Version = "v0.9.1" // the new panel
	if p := o.UpgradeProgress(ctx); p.Step != UpgradeDone {
		t.Errorf("landed step = %d", p.Step)
	}
}
