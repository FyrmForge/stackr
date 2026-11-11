package service

import (
	"context"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
)

// After a switchover the primary sits on a node the file lists as a standby:
// the cluster's node set (standbys plus the tile's own node) still matches, so
// the plan has nothing to change, and the export lists the other node.
func TestStandbyPlanAfterSwitchover(t *testing.T) {
	t.Parallel()
	ctx := withMayPlace(context.Background(), true)
	r := newFailRig(t, false)
	o, tl := r.n.orch, r.tile()
	ms, err := o.members.List(ctx, r.inst.ID)
	must(t, err)
	for _, m := range ms { // swap roles and nodes, as a switchover leaves them
		if m.Role == opmember.Primary {
			m.Role, m.NodeID = opmember.Replica, r.n.ctl.ID
		} else {
			m.Role, m.NodeID = opmember.Primary, "w1"
		}
		must(t, o.store.OperatedMembers.Update(ctx, m))
	}
	d, err := o.standbyPlan(ctx, tl, []promote.Standby{{Node: "w1"}})
	must(t, err)
	if d.Text != "" {
		t.Errorf("a switched cluster planned %q", d.Text)
	}
	ex, err := o.standbyExport(ctx, tl)
	must(t, err)
	if !slices.Equal(ex, []promote.Standby{{Node: "w1"}}) {
		t.Errorf("export = %+v", ex)
	}
	// a real removal still shows
	d, err = o.standbyPlan(ctx, tl, nil)
	must(t, err)
	if d.Text != "" { // the node left is the primary's: never removed
		t.Errorf("the primary's node was planned away: %q", d.Text)
	}
}

// A standby's sync mode is a plan row only when it differs, and is exported
// in the map form only when it is not auto.
func TestStandbySyncModeInFile(t *testing.T) {
	t.Parallel()
	ctx := withMayPlace(context.Background(), true)
	r := newFailRig(t, false)
	o, tl := r.n.orch, r.tile()
	d, err := o.standbyPlan(ctx, tl, []promote.Standby{{Node: "w1", Sync: "always"}})
	must(t, err)
	if len(d.Mode) != 1 || d.Mode[0] != (promote.Standby{Node: "w1", Sync: "always"}) || d.Text != "w1 sync always" {
		t.Fatalf("diff = %+v", d)
	}
	ms, _ := o.members.List(ctx, r.inst.ID)
	for _, m := range ms {
		if m.Role == opmember.Replica {
			_, err := o.members.SetSync(ctx, m, "always", m.SyncIn)
			must(t, err)
		}
	}
	d, err = o.standbyPlan(ctx, tl, []promote.Standby{{Node: "w1", Sync: "always"}})
	must(t, err)
	if d.Text != "" {
		t.Errorf("an applied mode planned %q", d.Text)
	}
	ex, err := o.standbyExport(ctx, tl)
	must(t, err)
	if !slices.Equal(ex, []promote.Standby{{Node: "w1", Sync: "always"}}) {
		t.Errorf("export = %+v", ex)
	}
	// the plain string form is auto again
	if d, err = o.standbyPlan(ctx, tl, []promote.Standby{{Node: "w1"}}); err != nil || len(d.Mode) != 1 || d.Mode[0].Sync != "auto" {
		t.Errorf("back to auto = %+v %v", d, err)
	}
	if _, err := o.standbyPlan(ctx, tl, []promote.Standby{{Node: "w1", Sync: "sometimes"}}); err == nil {
		t.Error("a bad mode passed")
	}
}
