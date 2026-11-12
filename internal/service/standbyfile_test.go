package service

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
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
	home := o.nodeName(ctx, r.n.ctl.ID)
	ex, err := o.standbyExport(ctx, tl)
	must(t, err)
	if !slices.Equal(ex, []promote.Standby{{Node: home}}) {
		t.Errorf("export = %+v", ex)
	}
	// the export applies as a no-op, whichever node is primary
	d, err := o.standbyPlan(ctx, tl, ex)
	must(t, err)
	if d.Text != "" {
		t.Errorf("a switched cluster planned %q", d.Text)
	}
	// an empty list removes the standby, never the primary's node
	d, err = o.standbyPlan(ctx, tl, nil)
	must(t, err)
	if d.Text != "−"+home {
		t.Errorf("empty list planned %q", d.Text)
	}
}

// The placement node holds no member: the export lists the standbys only, and
// applying it is a no-op instead of planning the placement node back in.
func TestStandbyExportApplyWithoutHomeMember(t *testing.T) {
	t.Parallel()
	ctx := withMayPlace(context.Background(), true)
	r := newFailRig(t, false)
	o, tl := r.n.orch, r.tile()
	ms, err := o.members.List(ctx, r.inst.ID)
	must(t, err)
	must(t, o.store.Nodes.Create(ctx, store.Node{ID: "w2", Name: "w2", Slug: "w2", Role: node.Worker, Tags: "[]", RangeCIDR: "10.210.0.0/16", Status: node.StatusOnline, CreatedAt: time.Now()}))
	for _, m := range ms { // primary on w1, the standby on w2: nothing on home
		if m.Role == opmember.Primary {
			m.NodeID = "w1"
		} else {
			m.NodeID = "w2"
		}
		must(t, o.store.OperatedMembers.Update(ctx, m))
	}
	ex, err := o.standbyExport(ctx, tl)
	must(t, err)
	if !slices.Equal(ex, []promote.Standby{{Node: o.nodeName(ctx, "w2")}}) {
		t.Fatalf("export = %+v", ex)
	}
	d, err := o.standbyPlan(ctx, tl, ex)
	must(t, err)
	if d.Text != "" {
		t.Errorf("export then apply planned %q", d.Text)
	}
}

// On a new instance the primary is created on home: a listed standby on home
// is dropped, not planned.
func TestStandbyPlanFreshDropsHome(t *testing.T) {
	t.Parallel()
	ctx := withMayPlace(context.Background(), true)
	r := newFailRig(t, false)
	o, tl := r.n.orch, r.tile()
	tl.ID = ""
	ctx = context.WithValue(ctx, placeAdminKey{}, true)
	d, err := o.standbyPlan(ctx, tl, []promote.Standby{{Node: o.nodeName(ctx, r.n.ctl.ID)}, {Node: "w1"}})
	must(t, err)
	if len(d.Add) != 1 || d.Add[0].Node != o.nodeName(ctx, "w1") {
		t.Errorf("diff = %+v", d)
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
