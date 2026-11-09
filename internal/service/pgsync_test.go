package service

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// addSecond adds a streaming standby on a new node w2.
func addSecond(t *testing.T, r *failRig) {
	t.Helper()
	ctx, o := context.Background(), r.n.orch
	must(t, o.store.Nodes.Create(ctx, store.Node{ID: "w2", Name: "w2", Slug: "w2", Role: node.Worker, Tags: "[]", RangeCIDR: "10.210.0.0/16",
		Status: node.StatusOnline, CreatedAt: time.Now()}))
	o.docker.(*DockerRouter).Set("w2", &sim{Docker: &subnetFake{Fake: r.n.wFake, nets: map[string]bool{}}, rig: r, node: "w2"})
	rep2, err := o.AddStandby(ctx, r.inst.TileID, "w2")
	must(t, err)
	o.vipAsync.Wait()
	_, err = o.members.SetPhase(ctx, rep2, opmember.Streaming)
	must(t, err)
}

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func pol() syncPolicy { return syncPolicy{On: true, LimitMS: 10} }

func auto(slot string, rtt float64, in, reachable bool) syncMember {
	return syncMember{Slot: slot, Mode: SyncAuto, RTT: rtt, Measured: true, Reachable: reachable, In: in}
}

func TestSyncNamesRender(t *testing.T) {
	t.Parallel()
	if got := mflow.SyncStandbyNames(nil); got != "" {
		t.Errorf("none = %q", got)
	}
	if got, want := mflow.SyncStandbyNames([]string{"stackr_a", "stackr_b"}), `ANY 1 ("stackr_a", "stackr_b")`; got != want {
		t.Errorf("names = %q, want %q", got, want)
	}
}

// A standby moves in only after it has been under the limit for the whole
// hold, and out only after it has been over it as long; a blip in between
// starts the clock again.
func TestSyncAutoHysteresis(t *testing.T) {
	t.Parallel()
	var c syncCtl
	m := auto("a", 2, false, true)
	if v := c.step(t0, pol(), []syncMember{m}); len(v.In) != 0 {
		t.Fatalf("in at once: %v", v.In)
	}
	if v := c.step(t0.Add(syncHold-time.Second), pol(), []syncMember{m}); len(v.In) != 0 {
		t.Fatalf("in before the hold: %v", v.In)
	}
	// a slow sample resets the clock
	c.step(t0.Add(syncHold), pol(), []syncMember{auto("a", 50, false, true)})
	if v := c.step(t0.Add(syncHold+time.Second), pol(), []syncMember{m}); len(v.In) != 0 {
		t.Fatalf("in after a reset: %v", v.In)
	}
	v := c.step(t0.Add(2*syncHold+2*time.Second), pol(), []syncMember{m})
	if !slices.Equal(v.In, []string{"a"}) || v.Protection != SyncProtected {
		t.Fatalf("verdict = %+v, want a in and protected", v)
	}
	// now in: over the limit, it stays until the hold has passed
	slow := auto("a", 40, true, true)
	now := t0.Add(10 * time.Minute)
	if v := c.step(now, pol(), []syncMember{slow}); len(v.In) != 1 {
		t.Fatalf("out at once: %v", v.In)
	}
	if v := c.step(now.Add(syncHold), pol(), []syncMember{slow}); len(v.In) != 0 || v.Protection != SyncUnprotected {
		t.Fatalf("verdict = %+v, want a out and unprotected", v)
	}
}

func TestSyncModes(t *testing.T) {
	t.Parallel()
	var c syncCtl
	ms := []syncMember{
		{Slot: "always", Mode: SyncAlways, RTT: 900, Measured: true, Reachable: true},
		{Slot: "never", Mode: SyncNever, RTT: 1, Measured: true, Reachable: true, In: true},
		{Slot: "unmeasured", Mode: SyncAuto, Reachable: true, In: true},
	}
	v := c.step(t0, pol(), ms)
	if want := []string{"always", "unmeasured"}; !slices.Equal(v.In, want) {
		t.Errorf("in = %v, want %v (always waits whatever the latency, never never, an unmeasured one stays as it was)", v.In, want)
	}
	if v := c.step(t0, syncPolicy{}, ms); len(v.In) != 0 || v.Protection != SyncOff {
		t.Errorf("off = %+v", v)
	}
}

// After a failover the node pairs changed: the first pass skips the hold.
func TestSyncFreshSkipsHold(t *testing.T) {
	t.Parallel()
	c := syncCtl{fresh: true}
	c.step(t0, pol(), nil) // nothing to judge yet: still fresh
	if v := c.step(t0, pol(), []syncMember{auto("a", 1, false, true)}); len(v.In) != 1 {
		t.Errorf("fresh pass left it out: %v", v.In)
	}
	if v := c.step(t0, pol(), []syncMember{auto("b", 1, false, true)}); len(v.In) != 0 {
		t.Errorf("fresh outlived its pass: %v", v.In)
	}
}

// Preferred: with no waited-for standby answering, the primary goes
// asynchronous after the grace and waits again when one answers.
func TestSyncPreferredFallsBackAndRecovers(t *testing.T) {
	t.Parallel()
	var c syncCtl
	up, down := auto("a", 1, true, true), auto("a", 1, true, false)
	if v := c.step(t0, pol(), []syncMember{up}); v.Protection != SyncProtected {
		t.Fatalf("up = %+v", v)
	}
	if v := c.step(t0.Add(time.Second), pol(), []syncMember{down}); v.Protection != SyncProtected || len(v.Names) != 1 {
		t.Fatalf("a blip inside the grace = %+v", v)
	}
	v := c.step(t0.Add(time.Second+syncGrace), pol(), []syncMember{down})
	if v.Protection != SyncUnprotected || len(v.Names) != 0 || len(v.In) != 1 {
		t.Fatalf("after the grace = %+v, want async names but a still in", v)
	}
	v = c.step(t0.Add(time.Minute), pol(), []syncMember{up})
	if v.Protection != SyncProtected || !slices.Equal(v.Names, []string{"a"}) {
		t.Fatalf("recovered = %+v", v)
	}
}

// Required keeps waiting: writes freeze, the names stay.
func TestSyncRequiredKeepsWaiting(t *testing.T) {
	t.Parallel()
	var c syncCtl
	p := pol()
	p.Required = true
	down := auto("a", 1, true, false)
	c.step(t0, p, []syncMember{down})
	v := c.step(t0.Add(syncGrace), p, []syncMember{down})
	if v.Protection != SyncBlocked || !slices.Equal(v.Names, []string{"a"}) {
		t.Fatalf("verdict = %+v, want blocked with the names kept", v)
	}
	// no standby at all (or all too far) is not a freeze: there is nothing to wait for
	if v := c.step(t0, p, nil); v.Protection != SyncUnprotected || len(v.Names) != 0 {
		t.Errorf("no standby = %+v", v)
	}
}

func TestSyncRendersIntoOwnedConf(t *testing.T) {
	t.Parallel()
	r := newFailRig(t, false)
	o := r.n.orch
	ctx := context.Background()
	inst, err := o.operated.SetSync(ctx, r.inst, true, 10, false)
	must(t, err)
	_, rep := r.members()
	_, err = o.members.SetSync(ctx, rep, SyncAlways, false)
	must(t, err)
	must(t, o.syncInstance(ctx, r.tile(), inst))
	if !r.saw(`ANY 1 ("` + mflow.SlotName(rep.ID) + `")`) {
		t.Errorf("the owned conf was not written with the names; log:\n%s", strings.Join(r.events, "\n"))
	}
	_, rep = r.members()
	if !rep.SyncIn {
		t.Error("the standby's flag does not say the primary waits for it")
	}
	// off: the setting goes
	inst, err = o.operated.SetSync(ctx, inst, false, 10, false)
	must(t, err)
	r.mu.Lock()
	r.events = nil
	r.mu.Unlock()
	must(t, o.syncInstance(ctx, r.tile(), inst))
	if r.saw("synchronous_standby_names") {
		t.Error("the setting stayed after turning it off")
	}
}

// A promote prefers a standby the primary waited for, then the highest LSN
// among them; one that was not waited for is the fallback.
func TestFailoverPrefersSyncStandby(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	addSecond(t, r)
	ms, _ := o.members.List(ctx, r.inst.ID)
	var reps []InstanceMember
	for _, m := range ms {
		if m.Role == opmember.Replica {
			reps = append(reps, m)
		}
	}
	// the standby on w2 is the more advanced one, but only w1's was waited for
	r.lsn["w1"], r.lsn["w2"] = "0/5000100", "0/9000000"
	setIn := func(in func(InstanceMember) bool) {
		for _, m := range reps {
			cur, err := o.members.Get(ctx, m.ID)
			must(t, err)
			_, err = o.members.SetSync(ctx, cur, SyncAuto, in(m))
			must(t, err)
		}
	}
	setIn(func(m InstanceMember) bool { return m.NodeID == "w1" })
	ms, _ = o.members.List(ctx, r.inst.ID)
	got, err := o.failoverTarget(ctx, r.tile(), ms, "", foSwitchover)
	must(t, err)
	if got.NodeID != "w1" {
		t.Errorf("target = %s, want w1 (the sync standby, though w2 is ahead)", got.NodeID)
	}
	// none waited for: the highest LSN as before
	setIn(func(InstanceMember) bool { return false })
	ms, _ = o.members.List(ctx, r.inst.ID)
	if got, err = o.failoverTarget(ctx, r.tile(), ms, "", foSwitchover); err != nil || got.NodeID != "w2" {
		t.Errorf("target = %v %v, want w2 by LSN", got.NodeID, err)
	}
	// both waited for: the higher LSN of the two
	setIn(func(InstanceMember) bool { return true })
	ms, _ = o.members.List(ctx, r.inst.ID)
	if got, err = o.failoverTarget(ctx, r.tile(), ms, "", foSwitchover); err != nil || got.NodeID != "w2" {
		t.Errorf("target = %v %v, want w2", got.NodeID, err)
	}
}

// After a switchover the slots are the new primary's: its setting names the
// old primary's slot, the standby it now has.
func TestSwitchoverRecomputesSyncNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	_, err := o.operated.SetSync(ctx, r.inst, true, 10, false)
	must(t, err)
	_, rep := r.members()
	_, err = o.members.SetSync(ctx, rep, SyncAlways, false)
	must(t, err)
	_, err = o.Switchover(ctx, r.inst.TileID, "w1")
	must(t, err)
	// the old primary is a standby now, and the next pass makes the primary wait for its slot
	_, newRep := r.members()
	_, err = o.members.SetSync(ctx, newRep, SyncAlways, false)
	must(t, err)
	must(t, o.syncInstance(ctx, r.tile(), r.inst))
	if !r.saw(`ANY 1 ("` + mflow.SlotName(newRep.ID) + `")`) {
		t.Errorf("the new primary was not told to wait for the old one's slot; log:\n%s", strings.Join(r.events, "\n"))
	}
}

// The webhook fires when the protection changes, in both directions, and
// not when switching on already protected.
func TestSyncAlertOnProtectionChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	h := newHook(t)
	alertsTo(t, o, h, "")
	tl, inst := r.tile(), r.inst
	o.syncAlert(ctx, tl, inst, syncVerdict{Protection: SyncProtected}, 1)
	o.syncAlert(ctx, tl, inst, syncVerdict{Protection: SyncProtected}, 1)
	o.syncAlert(ctx, tl, inst, syncVerdict{Protection: SyncUnprotected}, 1)
	o.syncAlert(ctx, tl, inst, syncVerdict{Protection: SyncUnprotected}, 1)
	if !o.hasProblem(ctx, "alert.pgmon.sync."+inst.ID) {
		t.Error("no problem while unprotected")
	}
	o.syncAlert(ctx, tl, inst, syncVerdict{Protection: SyncProtected}, 1)
	o.syncAlert(ctx, tl, inst, syncVerdict{Protection: SyncBlocked}, 1)
	if got, want := h.events(t, o), []string{"pgmon.sync:warning", "pgmon.sync:info", "pgmon.sync:critical"}; !slices.Equal(got, want) {
		t.Errorf("alerts = %v, want %v", got, want)
	}
	o.syncAlert(ctx, tl, inst, syncVerdict{Protection: SyncOff}, 1)
	if o.hasProblem(ctx, "alert.pgmon.sync."+inst.ID) {
		t.Error("the problem outlived turning it off")
	}
}
