package service

import (
	"context"
	"testing"
)

// A reconcile is done when observe is rightly down for a missing disk or runs
// again once the disk is back, and it needs no setting to say the data is on
// the disk (a plain box just gets the ordinary sync).
func TestObserveReconcile(t *testing.T) {
	ctx := context.Background()
	r := newObserveRig(t)
	if !r.orch.reconcileObserve(ctx, r.ctl()) || !r.orch.reconcileObserve(ctx, "w1") {
		t.Error("a plain box did not reconcile")
	}
	must(t, r.st.Settings.Set(ctx, observeDataDiskKey, "1"))
	setState(t, r.diskTest, r.row, "missing")
	if !r.orch.reconcileObserve(ctx, r.ctl()) {
		t.Error("observe rightly down for a missing disk is reconciled")
	}
	setState(t, r.diskTest, r.row, "ready")
	if !r.orch.reconcileObserve(ctx, r.ctl()) {
		t.Error("not reconciled once the disk is back")
	}
	if e := r.orch.observe.lastErr.Load(); e == nil || *e != "" {
		t.Errorf("lastErr = %v", e)
	}
}
