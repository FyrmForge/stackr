package service

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/vip"
)

// subnetFake asks for a subnet the way a daemon does for a network that is
// not there, which dockerfake never does.
type subnetFake struct {
	*dockerfake.Fake
	mu   sync.Mutex
	nets map[string]bool
}

func (f *subnetFake) EnsureNetwork(ctx context.Context, name string, sub netip.Prefix, l map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nets[name] {
		return nil
	}
	if !sub.IsValid() {
		return docker.ErrNeedsSubnet
	}
	f.nets[name] = true
	return f.Fake.EnsureNetwork(ctx, name, sub, l)
}

func (f *subnetFake) RemoveNetwork(ctx context.Context, name string) error {
	f.mu.Lock()
	delete(f.nets, name)
	f.mu.Unlock()
	return f.Fake.RemoveNetwork(ctx, name)
}

// pgOn is a postgres instance on the controller's dev env, with a primary
// member and a pause address on its node.
func pgOn(t *testing.T, n *twoNodes) store.OperatedInstance {
	t.Helper()
	ctx := context.Background()
	n.orch.docker.(*DockerRouter).Set("w1", &subnetFake{Fake: n.wFake, nets: map[string]bool{}})
	m, err := n.orch.operated.Create(ctx, n.devT.ID, "postgres", "stackr", "")
	must(t, err)
	_, err = n.orch.members.CreatePrimary(ctx, m.ID)
	must(t, err)
	n.ctlFake.Containers = append(n.ctlFake.Containers, docker.Container{ID: "p", State: "running", IPs: []string{"10.208.1.2"},
		Labels: map[string]string{tile.LabelTile: n.devT.ID, tile.LabelRole: "pause"}},
		// the primary itself: a standby's setup runs commands in it
		docker.Container{ID: "pg", Name: "web-dev", State: "running", Labels: map[string]string{tile.LabelTile: n.devT.ID, tile.LabelRole: "replica"}})
	return m
}

// A standby on w1 of the controller's instance: R4 refuses the primary's node
// and a node that holds a member; the row, w1's own network of the instance
// and its data volume are made there; the firewall opens replica network ->
// primary VIP:5432 (Out on w1, an accept on the controller) and the hosts
// line names the VIP; removing it undoes all of it.
func TestStandbyPlumbing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)

	if _, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, n.ctl.ID); !conflict(err) {
		t.Errorf("standby on the primary's node: %v, want a conflict", err)
	}
	if _, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "nope"); err == nil {
		t.Error("standby on an unknown node")
	}
	must(t, n.st.Environments.SetSubnet(ctx, n.dev.ID, "10.208.0.0/24"))
	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)
	if _, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1"); !conflict(err) {
		t.Errorf("second standby on w1: %v, want a conflict", err)
	}
	// a block of the cluster pool, clear of the env's
	sub, err := netip.ParsePrefix(r.Subnet)
	if err != nil || !mesh.Supernet.Contains(sub.Addr()) || sub.Overlaps(netip.MustParsePrefix("10.208.0.0/24")) {
		t.Fatalf("subnet = %q %v, want a block of the pool other than the env's", r.Subnet, err)
	}
	net := operated.Network(m.ID)
	if !sawCall(n.wFake, "EnsureNetwork("+net+")") || !sawCall(n.wFake, "CreateVolume("+r.Volume+")") {
		t.Errorf("w1 calls: %v", callNames(n.wFake))
	}
	if sawCall(n.ctlFake, "CreateVolume(") {
		t.Error("the standby's volume was made on the controller")
	}
	if got := n.wFake.Created[len(n.wFake.Created)-1].Labels[LabelMember]; got != r.ID {
		t.Errorf("volume label = %q", got)
	}

	want := vip.Flow{Src: sub, VIP: netip.MustParseAddr("10.208.1.2"), Port: 5432}
	if got := n.orch.linkFlows(ctx, n.ctl); !slices.Contains(got, want) {
		t.Errorf("controller flows = %+v, want %+v", got, want)
	}
	w1, err := n.st.Nodes.Get(ctx, "w1")
	must(t, err)
	want.Out = true
	if got := n.orch.vipBase(ctx, w1).Flows; !slices.Contains(got, want) {
		t.Errorf("w1 flows = %+v, want %+v", got, want)
	}
	if h, err := n.orch.memberHost(ctx, r, "w1"); err != nil || h != operated.Alias(n.devT.Slug, m.ID)+":10.208.1.2" {
		t.Errorf("member host = %q %v", h, err)
	}

	if p, err := n.orch.Protection(ctx, n.devT.ID); err != nil || p != "Standby on w1" {
		t.Errorf("protection = %q %v", p, err)
	}
	ms, err := n.orch.InstanceMembers(ctx, n.devT.ID)
	if err != nil || len(ms) != 2 || ms[0].Role != opmember.Primary || ms[1].ID != r.ID {
		t.Errorf("members = %+v %v", ms, err)
	}
	if err := n.orch.engines.Teardown(ctx, n.devT, true, io.Discard); !conflict(err) {
		t.Errorf("teardown with a standby: %v, want a conflict", err)
	}

	must(t, n.orch.RemoveStandby(ctx, r.ID))
	if !sawCall(n.wFake, "RemoveVolume("+r.Volume+")") || !sawCall(n.wFake, "RemoveNetwork("+net+")") {
		t.Errorf("w1 calls after remove: %v", callNames(n.wFake))
	}
	if got := n.orch.linkFlows(ctx, n.ctl); slices.Contains(got, vip.Flow{Src: sub, VIP: want.VIP, Port: 5432}) {
		t.Error("the flow outlived the standby")
	}
	if p, _ := n.orch.Protection(ctx, n.devT.ID); p != "No standby" {
		t.Errorf("protection after remove = %q", p)
	}
	if err := n.orch.RemoveStandby(ctx, r.ID); err == nil {
		t.Error("removed a standby twice")
	}
}

// The panel starts members: a stopped postgres container comes back on the
// tick, one the panel stopped or a fenced one does not, and a fenced primary
// cannot be started by the verb either.
func TestStartMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)
	n.ctlFake.Containers = append(n.ctlFake.Containers, docker.Container{ID: "pg1", State: "exited",
		Labels: map[string]string{tile.LabelTile: n.devT.ID, tile.LabelRole: "replica"}})
	n.wFake.Containers = append(n.wFake.Containers, docker.Container{ID: "rep1", State: "exited",
		Labels: map[string]string{LabelMember: r.ID}})

	ms, err := n.orch.members.List(ctx, m.ID)
	must(t, err)
	prim, _ := opmember.PrimaryOf(ms)
	_, err = n.orch.members.SetFenced(ctx, r, true)
	must(t, err)
	_, err = n.orch.members.SetState(ctx, prim, opmember.Stopped)
	must(t, err)
	n.orch.startMembers(ctx)
	if sawCall(n.ctlFake, "Start(") || sawCall(n.wFake, "Start(") {
		t.Errorf("started a stopped or fenced member: %v %v", callNames(n.ctlFake), callNames(n.wFake))
	}

	_, err = n.orch.members.SetState(ctx, prim, opmember.Running)
	must(t, err)
	n.orch.startMembers(ctx)
	if !sawCall(n.ctlFake, "Start(pg1)") || sawCall(n.wFake, "Start(") {
		t.Errorf("after the primary is wanted up: %v %v", callNames(n.ctlFake), callNames(n.wFake))
	}

	must(t, n.orch.FenceMember(ctx, r.ID, false))
	n.orch.startMembers(ctx)
	if !sawCall(n.wFake, "Start(rep1)") {
		t.Errorf("an unfenced standby stays down: %v", callNames(n.wFake))
	}

	must(t, n.orch.FenceMember(ctx, prim.ID, true))
	if err := n.orch.mayStartTile(ctx, n.devT); !conflict(err) {
		t.Errorf("start of a fenced primary: %v, want a conflict", err)
	}
	must(t, n.orch.FenceMember(ctx, prim.ID, false))
	if err := n.orch.mayStartTile(ctx, n.devT); err != nil {
		t.Errorf("start after the fence is cleared: %v", err)
	}
}

// Nothing restores a postgres volume while a standby may be alive; only a
// positively stopped one does not count. A fence says nothing about the
// container.
func TestRestoreRefusedWithLiveMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	_, _, err := n.orch.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: n.dev.ID}, n.devT.Slug+"-data", &m.ID)
	must(t, err)
	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)

	_, _, err = n.orch.restoreThenDeploy(ctx, n.dev.ID, nil)
	if !conflict(err) || !strings.Contains(err.Error(), "standby that may be running on node w1") {
		t.Fatalf("restore with a live standby: %v", err)
	}
	must(t, n.orch.FenceMember(ctx, r.ID, true))
	if _, _, err = n.orch.restoreThenDeploy(ctx, n.dev.ID, nil); !conflict(err) {
		t.Errorf("restore with a fenced standby: %v, want a conflict", err)
	}
	// the placement restore asks before the env changes node
	e, err := n.st.Environments.Get(ctx, n.dev.ID)
	must(t, err)
	var warn []error
	if _, err := n.orch.pmRestoreEnv(ctx, nil, e, "w1", &warn); !conflict(err) {
		t.Errorf("placement restore with a live standby: %v, want a conflict", err)
	}
	if got, _ := n.st.Environments.Get(ctx, n.dev.ID); got.NodeID != e.NodeID {
		t.Errorf("env node = %q after the refused restore, want %q", got.NodeID, e.NodeID)
	}
	_, err = n.orch.members.SetState(ctx, r, opmember.Stopped)
	must(t, err)
	if _, _, err = n.orch.restoreThenDeploy(ctx, n.dev.ID, nil); err != nil {
		t.Errorf("restore with only a stopped standby: %v", err)
	}
}

// A single database keeps the tile's restart policy; the first standby makes
// the primary "no" live and records it wanted up, the last one's going gives
// the policy back. Without a standby the tick never starts it.
func TestStandbyRestartPolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	n.ctlFake.Containers = append(n.ctlFake.Containers, docker.Container{ID: "pg1", State: "running",
		Labels: map[string]string{tile.LabelTile: n.devT.ID, tile.LabelRole: "replica"}})
	must(t, n.st.Environments.SetSubnet(ctx, n.dev.ID, "10.208.0.0/24"))

	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)
	if !sawCall(n.ctlFake, "UpdateRestart(pg1, no)") {
		t.Errorf("first standby: %v", callNames(n.ctlFake))
	}
	ms, _ := n.orch.members.List(ctx, m.ID)
	if p, _ := opmember.PrimaryOf(ms); p.State != opmember.Running {
		t.Errorf("primary state = %q, want running (its container runs)", p.State)
	}
	must(t, n.orch.RemoveStandby(ctx, r.ID))
	if !sawCall(n.ctlFake, "UpdateRestart(pg1, unless-stopped)") {
		t.Errorf("last standby gone: %v", callNames(n.ctlFake))
	}

	// no standby: an exited primary is not the tick's to start
	n.ctlFake.Containers[len(n.ctlFake.Containers)-1].State = "exited"
	n.orch.startMembers(ctx)
	if sawCall(n.ctlFake, "Start(") {
		t.Errorf("started a single instance: %v", callNames(n.ctlFake))
	}
}

// A restore or a promote that rewrites the primary's data is refused while
// the instance has a standby, a promote also over a fenced primary.
func TestStandbyBlocksRestoreAndPromote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	if err := n.orch.promoteAllowed(ctx, n.devT); err != nil {
		t.Fatalf("promote of a plain instance: %v", err)
	}
	_, err := n.orch.members.AddReplica(ctx, m.ID, "x", "w1")
	must(t, err)
	if err := n.orch.promoteAllowed(ctx, n.devT); !conflict(err) {
		t.Errorf("promote with a standby: %v, want a conflict", err)
	}
	if err := n.orch.noStandbys(ctx, m.ID); !conflict(err) {
		t.Errorf("tar restore with a standby: %v, want a conflict", err)
	}
	ms, _ := n.orch.members.List(ctx, m.ID)
	for _, x := range ms {
		if x.Role == opmember.Replica {
			must(t, n.orch.RemoveStandby(ctx, x.ID))
		}
	}
	prim, _ := opmember.PrimaryOf(ms)
	must(t, n.orch.FenceMember(ctx, prim.ID, true))
	if err := n.orch.promoteAllowed(ctx, n.devT); !conflict(err) {
		t.Errorf("promote over a fenced primary: %v, want a conflict", err)
	}
}

// The volume restore guard asks for standbys; a dump restore is exempt.
func TestVolumeRestoreRefusedWithStandby(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newPITRRig(t)
	o := r.orch
	_, err := o.members.AddReplica(ctx, r.m.ID, "a", "b")
	must(t, err)
	dest, err := o.pitrDest(ctx, r.org, r.data)
	must(t, err)
	tar, err := o.backups.StartIn(ctx, backup.KindVolume, &r.data.ID, nil, dest.ID, "manual", nil)
	must(t, err)
	tar, err = o.backups.Finish(ctx, tar, "k/20261107-000000-data.tar.gz.age", 1, nil)
	must(t, err)
	if err := o.pitrVolumeRestoreOK(ctx, tar.ID, r.data); !conflict(err) {
		t.Errorf("tar restore with a standby: %v, want a conflict", err)
	}
}

// Two standbys at most, even when the adds race.
func TestAddReplicaLimitRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	var wg sync.WaitGroup
	for _, node := range []string{"x1", "x2", "x3", "x4", "x5"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = n.orch.members.AddReplica(ctx, m.ID, "p", node)
		}()
	}
	wg.Wait()
	ms, err := n.orch.members.List(ctx, m.ID)
	must(t, err)
	got := 0
	for _, x := range ms {
		if x.Role == opmember.Replica {
			got++
		}
	}
	if got != opmember.MaxReplicas {
		t.Errorf("%d standbys after the race, want %d", got, opmember.MaxReplicas)
	}
}

// An env cannot move onto a node that holds its instance's standby: the
// plan fails and the move's own start refuses before the node changes.
func TestMoveOntoStandbyNodeRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	pgOn(t, n)
	must(t, n.st.Environments.SetSubnet(ctx, n.dev.ID, "10.208.0.0/24"))
	_, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)

	cs, err := n.orch.moveChecks(ctx, []string{n.dev.ID}, n.ctl.ID, "w1")
	must(t, err)
	failed := ""
	for _, c := range cs {
		if c.Name == "standby" && !c.OK {
			failed = c.Detail
		}
	}
	if failed == "" {
		t.Errorf("checks = %+v, want a failed standby check", cs)
	}
	if err := (moveEnv{n.orch}).Start(ctx, n.dev.ID, "w1"); !conflict(err) {
		t.Errorf("move start onto the standby's node: %v, want a conflict", err)
	}
	if got, _ := n.st.Environments.Get(ctx, n.dev.ID); got.NodeID != n.ctl.ID {
		t.Errorf("env node = %q, want it unchanged", got.NodeID)
	}
}

// A worker that stops answering must not hold the instance's topology lock
// past the attempt deadline: the tick gives up and the next one can run.
func TestFinishRemovalsGivesUpOnAHungNode(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	pgOn(t, n)
	must(t, n.st.Environments.SetSubnet(ctx, n.dev.ID, "10.208.0.0/24"))
	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)
	n.wFake.Err = map[string]error{"RemoveNetwork": errors.New("active endpoints")}
	if err := n.orch.RemoveStandby(ctx, r.ID); err == nil {
		t.Fatal("remove succeeded with the network stuck")
	}
	old := tickAttempt
	tickAttempt = 50 * time.Millisecond
	defer func() { tickAttempt = old }()
	n.wFake.Edit(func() { n.wFake.BlockList = true })
	done := make(chan struct{})
	go func() { n.orch.finishRemovals(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("finishRemovals still blocked on the hung node")
	}
	mu := n.orch.topoLock(r.InstanceID)
	if !mu.TryLock() {
		t.Fatal("topology lock still held")
	}
	mu.Unlock()
}

// A standby whose network will not go keeps its row (and subnet) as
// 'removing'; the tick finishes it once the network goes.
func TestRemoveStandbyKeepsRowUntilNetworkGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	must(t, n.st.Environments.SetSubnet(ctx, n.dev.ID, "10.208.0.0/24"))
	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)

	n.wFake.Err = map[string]error{"RemoveNetwork": errors.New("active endpoints")}
	if err := n.orch.RemoveStandby(ctx, r.ID); err == nil {
		t.Fatal("remove succeeded with the network stuck")
	}
	got, err := n.orch.members.Get(ctx, r.ID)
	if err != nil || got.State != opmember.Removing || got.Subnet != r.Subnet {
		t.Fatalf("row after a failed removal = %+v %v, want removing with its subnet", got, err)
	}
	n.orch.finishRemovals(ctx) // still stuck
	if _, err := n.orch.members.Get(ctx, r.ID); err != nil {
		t.Errorf("row gone while the network stays: %v", err)
	}
	delete(n.wFake.Err, "RemoveNetwork")
	n.orch.finishRemovals(ctx)
	if _, err := n.orch.members.Get(ctx, r.ID); err == nil {
		t.Error("row kept after the network went")
	}
	if ms, _ := n.orch.members.List(ctx, m.ID); len(ms) != 1 {
		t.Errorf("members = %+v, want the primary only", ms)
	}
}

// A standby's network is announced to the mesh as its node's subnet, so the
// primary's node can route the standby's replies.
func TestStandbySubnetIsMeshed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	pgOn(t, n)
	must(t, n.st.Environments.SetSubnet(ctx, n.dev.ID, "10.208.0.0/24"))
	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)
	n.orch.mesh = newMeshSvc(n.st, func(string) (MeshNode, bool) { return nil, false })
	in, err := n.orch.mesh.gather(ctx)
	must(t, err)
	for _, x := range in.Nodes {
		if x.ID == r.NodeID && !slices.ContainsFunc(x.Subnets, func(p netip.Prefix) bool { return p.String() == r.Subnet }) {
			t.Errorf("node subnets %v lack the standby's %s", x.Subnets, r.Subnet)
		}
	}
}

// Deleting an instance whose primary a failover moved to w1 removes the
// member's container, its pooler and the network there, not just the tile's
// own objects on the env's node.
func TestDeleteRemovesTheRelocatedPrimary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	ms, err := n.orch.members.List(ctx, m.ID)
	must(t, err)
	p, _ := opmember.PrimaryOf(ms)
	p.NodeID, p.Volume = "w1", "stackr-member-vol"
	must(t, n.st.OperatedMembers.Update(ctx, p))
	n.wFake.Containers = []docker.Container{
		{ID: "mc", State: "running", Labels: map[string]string{LabelMember: p.ID}},
		{ID: "pool", State: "running", Labels: map[string]string{tile.LabelTile: n.devT.ID, tile.LabelRole: mflow.PoolerRole}},
	}
	rel, ok := n.orch.relocated(ctx, n.devT.ID)
	if !ok {
		t.Fatal("the primary is not seen as relocated")
	}
	must(t, n.orch.dropRelocated(ctx, n.devT, rel))
	for _, c := range []string{"StopRemove(mc)", "StopRemove(pool)"} {
		if !sawCall(n.wFake, c) {
			t.Errorf("%s missing: %v", c, callNames(n.wFake))
		}
	}
}

// A stuck old primary's tile container still holds the member's volume: a
// removal takes it first, so the row does not stay 'removing'.
func TestRemoveStandbyDropsTheOldTileContainer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	pgOn(t, n)
	must(t, n.st.Environments.SetSubnet(ctx, n.dev.ID, "10.208.0.0/24"))
	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)
	n.wFake.Containers = append(n.wFake.Containers, docker.Container{ID: "old-tile", State: "exited",
		Labels: map[string]string{tile.LabelTile: n.devT.ID, tile.LabelRole: "replica"}})
	must(t, n.orch.RemoveStandby(ctx, r.ID))
	if !sawCall(n.wFake, "StopRemove(old-tile)") {
		t.Errorf("the old tile container stayed: %v", callNames(n.wFake))
	}
	if _, err := n.orch.members.Get(ctx, r.ID); err == nil {
		t.Error("row kept")
	}
}
