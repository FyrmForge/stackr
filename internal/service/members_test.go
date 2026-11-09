package service

import (
	"context"
	"io"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
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
		Labels: map[string]string{tile.LabelTile: n.devT.ID, tile.LabelRole: "pause"}})
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

	if _, err := n.orch.AddStandby(ctx, n.devT.ID, n.ctl.ID); !conflict(err) {
		t.Errorf("standby on the primary's node: %v, want a conflict", err)
	}
	if _, err := n.orch.AddStandby(ctx, n.devT.ID, "nope"); err == nil {
		t.Error("standby on an unknown node")
	}
	must(t, n.st.Environments.SetSubnet(ctx, n.dev.ID, "10.208.0.0/24"))
	r, err := n.orch.AddStandby(ctx, n.devT.ID, "w1")
	must(t, err)
	if _, err := n.orch.AddStandby(ctx, n.devT.ID, "w1"); !conflict(err) {
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
	r, err := n.orch.AddStandby(ctx, n.devT.ID, "w1")
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

// Nothing restores a postgres volume while a standby may be alive; a fenced
// or stopped one does not count.
func TestRestoreRefusedWithLiveMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	_, _, err := n.orch.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: n.dev.ID}, n.devT.Slug+"-data", &m.ID)
	must(t, err)
	r, err := n.orch.AddStandby(ctx, n.devT.ID, "w1")
	must(t, err)

	_, _, err = n.orch.restoreThenDeploy(ctx, n.dev.ID, nil)
	if !conflict(err) || !strings.Contains(err.Error(), "instance has a live member on node w1") {
		t.Fatalf("restore with a live standby: %v", err)
	}
	must(t, n.orch.FenceMember(ctx, r.ID, true))
	if _, _, err = n.orch.restoreThenDeploy(ctx, n.dev.ID, nil); err != nil {
		t.Errorf("restore with only a fenced standby: %v", err)
	}
}
