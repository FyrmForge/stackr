package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A promote target that replayed less than another usable standby is refused,
// an unreadable position refuses too, and the drawer lists the most advanced
// standby first.
func TestPromoteRefusesLaggingTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	addSecond(t, r)
	o := r.n.orch
	ms, _ := o.members.List(ctx, r.inst.ID)
	var w1, w2 InstanceMember
	for _, m := range ms {
		switch m.NodeID {
		case "w1":
			w1 = m
		case "w2":
			w2 = m
		}
	}
	r.lsn["w1"], r.lsn["w2"] = "0/5000100", "0/9000000"
	if err := o.targetBehind(ctx, r.tile(), ms, w1); err == nil || !strings.Contains(err.Error(), "w2") {
		t.Errorf("lagging target: %v", err)
	}
	if err := o.targetBehind(ctx, r.tile(), ms, w2); err != nil {
		t.Errorf("most advanced target refused: %v", err)
	}
	r.lsn["w2"] = ""
	if err := o.targetBehind(ctx, r.tile(), ms, w1); err == nil {
		t.Error("an unreadable position passed")
	}
	r.lsn["w2"] = "0/9000000"
	r.sims["ctl"].set("pg", "exited")
	fo, _, err := o.FailoverOptionsOf(ctx, r.inst.TileID)
	must(t, err)
	if len(fo.Promote) != 2 || fo.Promote[0] != "w2" {
		t.Errorf("promote order = %v, want w2 first", fo.Promote)
	}
}

// A failover whose cursor holds an error raises a problem, cleared when it ends.
func TestStuckFailoverProblem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	key := "alert." + AlertPGFailoverStuck + "." + r.inst.ID
	f := failover{Kind: foPromoted, Step: stepStop, Started: time.Now()}
	must(t, o.saveFailover(ctx, r.inst.ID, f))
	o.observePrimaries(ctx)
	if o.hasProblem(ctx, key) {
		t.Error("a fresh failover without error alerted")
	}
	f.Error = "boom"
	must(t, o.saveFailover(ctx, r.inst.ID, f))
	o.observePrimaries(ctx)
	if !o.hasProblem(ctx, key) {
		t.Fatal("no problem for a failover holding an error")
	}
	must(t, o.store.Settings.Delete(ctx, failoverKey(r.inst.ID)))
	o.observePrimaries(ctx)
	if o.hasProblem(ctx, key) {
		t.Error("the problem stayed after the failover ended")
	}
	f.Error, f.Started = "", time.Now().Add(-2*stuckAfter)
	must(t, o.saveFailover(ctx, r.inst.ID, f))
	o.observePrimaries(ctx)
	if !o.hasProblem(ctx, key) {
		t.Error("no problem for a failover open too long")
	}
}

// A node whose primaries cannot be listed is held; it is not a global error.
func TestLeaseListErrorIsPerNode(t *testing.T) {
	t.Parallel()
	r := newFailRig(t, true)
	r.mu.Lock()
	r.listErr = errors.New("docker down")
	r.mu.Unlock()
	w := r.n.orch.leaseWanted(context.Background())
	if w.err != nil || !w.hold["w1"] {
		t.Errorf("err = %v hold = %v, want only w1 held", w.err, w.hold)
	}
}

// A stopped or catching-up standby is not a competitor: promoting the healthy
// one is not refused because the other cannot be read.
func TestTargetBehindIgnoresStoppedStandby(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	addSecond(t, r)
	o := r.n.orch
	ms, _ := o.members.List(ctx, r.inst.ID)
	var w1, w2 InstanceMember
	for _, m := range ms {
		switch m.NodeID {
		case "w1":
			w1 = m
		case "w2":
			w2 = m
		}
	}
	_, err := o.members.SetPhase(ctx, w1, "streaming")
	must(t, err)
	r.lsn["w1"], r.lsn["w2"] = "0/5000100", "" // w2 unreadable
	ms, _ = o.members.List(ctx, r.inst.ID)
	w1, _ = o.members.Get(ctx, w1.ID)
	if err := o.targetBehind(ctx, r.tile(), ms, w1); err == nil {
		t.Fatal("an unreadable running standby was ignored")
	}
	_, err = o.members.SetState(ctx, w2, "stopped")
	must(t, err)
	ms, _ = o.members.List(ctx, r.inst.ID)
	if err := o.targetBehind(ctx, r.tile(), ms, w1); err != nil {
		t.Errorf("a stopped standby blocked the promote: %v", err)
	}
}

// A stalled failover alerts even when its only other member is a fenced
// replica waiting for a rewind.
func TestStuckFailoverAlertsWithOnlyFencedReplica(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	_, rep := r.members()
	_, err := o.members.SetFenced(ctx, rep, true)
	must(t, err)
	cur, _ := o.members.Get(ctx, rep.ID)
	_, err = o.members.SetPhase(ctx, cur, "rewind")
	must(t, err)
	must(t, o.saveFailover(ctx, r.inst.ID, failover{Kind: foPromoted, Step: stepSlots, Error: "boom", Started: time.Now()}))
	o.observePrimaries(ctx)
	if !o.hasProblem(ctx, "alert."+AlertPGFailoverStuck+"."+r.inst.ID) {
		t.Error("no alert for a stalled failover")
	}
}

// Placing a standby through the stack file needs an owner's approval.
func TestStandbyPlanNeedsOwner(t *testing.T) {
	t.Parallel()
	r := newFailRig(t, false)
	o, tl := r.n.orch, r.tile()
	if _, _, err := o.standbyPlan(context.Background(), tl, nil); err == nil {
		t.Error("removing the standby without an owner passed")
	}
	if _, _, err := o.standbyPlan(withMayPlace(context.Background(), true), tl, nil); err != nil {
		t.Errorf("owner refused: %v", err)
	}
	if _, _, err := o.standbyPlan(context.Background(), tl, []string{"w1"}); err != nil {
		t.Errorf("unchanged list refused: %v", err)
	}
}

// With the primary dead every standby is "starting", not streaming; the lagging
// one is still refused against the running, more advanced one.
func TestTargetBehindCountsStartingStandbys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	addSecond(t, r)
	o := r.n.orch
	ms, _ := o.members.List(ctx, r.inst.ID)
	var w1 InstanceMember
	for _, m := range ms {
		if m.Role == "replica" {
			_, err := o.members.SetPhase(ctx, m, "starting")
			must(t, err)
		}
		if m.NodeID == "w1" {
			w1 = m
		}
	}
	r.sims["ctl"].set("pg", "exited")
	r.lsn["w1"], r.lsn["w2"] = "0/5000100", "0/9000000"
	ms, _ = o.members.List(ctx, r.inst.ID)
	w1, _ = o.members.Get(ctx, w1.ID)
	if err := o.targetBehind(ctx, r.tile(), ms, w1); err == nil || !strings.Contains(err.Error(), "w2") {
		t.Errorf("lagging starting standby passed: %v", err)
	}
}

// A node no placement opens to the org takes no standby, promote or switchover
// target from an org owner; an admin may.
func TestStandbyNodeMustBeOpenToOrg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	must(t, o.store.Nodes.Create(ctx, store.Node{ID: "w2", Name: "w2", Slug: "w2", Role: node.Worker, Tags: "[]", RangeCIDR: "10.210.0.0/16",
		Status: node.StatusOnline, CreatedAt: time.Now()}))
	o.docker.(*DockerRouter).Set("w2", &sim{Docker: &subnetFake{Fake: r.n.wFake, nets: map[string]bool{}}, rig: r, node: "w2"})
	addPlacement(t, r.n, "w2p", "w2", false) // open to no org
	own := ownerP(acmeID(t, r.n))
	if _, err := o.AddStandby(ctx, own, r.inst.TileID, "w2"); err == nil {
		t.Fatal("owner added a standby on a node closed to the org")
	}
	ms, _ := o.members.List(ctx, r.inst.ID)
	for _, kind := range []string{foSwitchover, foPromoted} {
		if _, err := o.failoverTarget(ctx, own, r.tile(), ms, "w2", kind); err == nil {
			t.Errorf("%s: owner picked a node closed to the org", kind)
		}
	}
	if _, _, err := o.standbyPlan(withMayPlace(ctx, true), r.tile(), []string{"w1", "w2"}); err == nil {
		t.Error("standbyPlan accepted a closed node without admin")
	}
	if _, err := o.AddStandby(ctx, adminP(), r.inst.TileID, "w2"); err != nil {
		t.Fatalf("admin refused: %v", err)
	}
}
