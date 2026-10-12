package service

import (
	"context"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/exits"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
	"github.com/FyrmForge/stackr/internal/worker"
)

func tnode(id string, slot int, pub string, envs ...meshEnvIn) meshNodeIn {
	r := netip.MustParsePrefix("10." + strconv.Itoa(208+slot) + ".0.0/16")
	return meshNodeIn{
		Node:    mesh.Node{ID: id, PublicKey: "wg-" + id, Range: r, Port: MeshWGPort, Public: []netip.Addr{netip.MustParseAddr(pub)}},
		Name:    id,
		ExitKey: "ex-" + id,
		Envs:    envs,
	}
}

func TestBuildMeshExits(t *testing.T) {
	in := meshIn{Nodes: []meshNodeIn{
		tnode("c", 5, "198.51.100.1"),
		tnode("a", 0, "203.0.113.1",
			meshEnvIn{Subnet: "10.208.1.0/24", Ladder: []string{"c", "a"}},
			meshEnvIn{Subnet: "10.208.2.0/24", Ladder: []string{"c", "a"}},
			meshEnvIn{Subnet: "10.208.3.0/24", Ladder: []string{"b"}}),
		tnode("b", 1, "203.0.113.2"),
	}}
	specs := buildMesh(in)

	a, c := specs["a"], specs["c"]
	if len(a.Peers) != 2 || a.ListenPort != MeshWGPort || a.Address.String() != "10.207.255.1/24" {
		t.Fatalf("a mesh = %+v", a)
	}
	// Two subjects with disjoint pref blocks; the first ladder lists the own uplink last.
	if len(a.Ladders) != 2 {
		t.Fatalf("ladders = %+v", a.Ladders)
	}
	l1, l0 := a.Ladders[0], a.Ladders[1] // keys sort "b" before "c,a"
	if l1.Subject.PrefBase+2*len(l1.Subject.Sources) > l0.Subject.PrefBase {
		t.Errorf("pref blocks overlap: %d (%d) and %d", l0.Subject.PrefBase, len(l0.Subject.Sources), l1.Subject.PrefBase)
	}
	// Links: one per exit node, scoped to the sources routed over it.
	if len(a.Links) != 2 {
		t.Fatalf("links = %+v", a.Links)
	}
	byIface := map[string]exits.Link{}
	for _, l := range a.Links {
		byIface[l.Iface] = l
	}
	lc := byIface[exits.Iface("c")]
	if strings.Join(lc.Sources, ",") != "10.208.1.0/24,10.208.2.0/24" || lc.PeerKey != "ex-c" || lc.Table != exitTableLink+5 {
		t.Errorf("link to c = %+v", lc)
	}
	// The exit side serves exactly those subnets, from the client's slot.
	if len(c.Serves) != 1 {
		t.Fatalf("c serves = %+v", c.Serves)
	}
	sv := c.Serves[0]
	if sv.Iface != exits.Iface("a") || strings.Join(sv.Subnets, ",") != "10.208.1.0/24,10.208.2.0/24" ||
		sv.PeerKey != "ex-a" || sv.Mark != exitMarkBase || sv.Table != exitTableServ || sv.Port != lc.Port {
		t.Errorf("serve = %+v", sv)
	}
	// Both ends agree on the /30: client .1, exit .2, and the rung pings the exit end.
	ca, sa := netip.MustParsePrefix(lc.Addr), netip.MustParsePrefix(sv.Addr)
	if ca.Masked() != sa.Masked() || ca.Addr() == sa.Addr() {
		t.Errorf("link %s and serve %s are not one /30", lc.Addr, sv.Addr)
	}
	var rung exits.Rung
	for _, r := range l0.Rungs {
		if r.NodeID == "c" {
			rung = r
		}
	}
	if rung.Target != sa.Addr().String() || rung.Iface != lc.Iface {
		t.Errorf("rung = %+v", rung)
	}
	// The own uplink rung installs nothing.
	if !l0.Rungs[1].Own {
		t.Errorf("rungs = %+v", l0.Rungs)
	}
	if len(specs["b"].Links) != 0 || len(specs["b"].Ladders) != 0 || len(specs["b"].Serves) != 1 {
		t.Errorf("b = %+v", specs["b"])
	}
	// Unchanged input gives the same plan (addresses never shuffle).
	if again := buildMesh(in); len(again["a"].Links) != 2 || again["a"].Links[0].Addr != a.Links[0].Addr {
		t.Error("plan not deterministic")
	}
}

func TestBuildMeshWaitsForExitKey(t *testing.T) {
	x := tnode("x", 2, "203.0.113.9")
	x.ExitKey = ""
	in := meshIn{Nodes: []meshNodeIn{tnode("a", 0, "203.0.113.1", meshEnvIn{Subnet: "10.208.1.0/24", Ladder: []string{"x"}}), x}}
	if s := buildMesh(in); len(s["a"].Links) != 0 || len(s["x"].Serves) != 0 {
		t.Errorf("link built without the exit's key: %+v", s)
	}
}

type fakeMeshNode struct {
	mu     sync.Mutex
	pushed []worker.MeshSpec
	fail   error
}

func (f *fakeMeshNode) Facts(context.Context) (worker.Facts, error) {
	return worker.Facts{Interfaces: []worker.Iface{{Name: "eth0", Prefixes: []string{"203.0.113.7/24"}, Public: true}}}, nil
}
func (f *fakeMeshNode) MeshInfo(context.Context) (worker.MeshInfo, error) {
	return worker.MeshInfo{ExitPublic: "ex"}, nil
}
func (f *fakeMeshNode) ApplyMesh(_ context.Context, s worker.MeshSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.pushed = append(f.pushed, s)
	return nil
}

func TestMeshSyncPushesOnChangeAndRemoveNode(t *testing.T) {
	ctx := context.Background()
	st := storetest.Store(t)
	ctrl := storetest.Controller(t, st)
	wk := ctrl
	wk.ID, wk.Name, wk.Slug, wk.Role = "w1", "w1", "w1", "worker"
	wk.RangeCIDR, wk.WGPublicKey = "10.214.0.0/16", "wgkey-w1"
	if err := st.Nodes.Create(ctx, wk); err != nil {
		t.Fatal(err)
	}
	fakes := map[string]*fakeMeshNode{ctrl.ID: {}, wk.ID: {}}
	m := newMeshSvc(st, func(id string) (MeshNode, bool) { f, ok := fakes[id]; return f, ok })

	if err := m.Sync(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	if n := len(fakes[ctrl.ID].pushed); n != 1 {
		t.Fatalf("controller pushes = %d", n)
	}
	if p := fakes[ctrl.ID].pushed[0].Peers; len(p) != 1 || p[0].NodeID != "w1" || len(p[0].Candidates) == 0 {
		t.Errorf("controller peers = %+v", p)
	}
	// Nothing changed: no push. The timer pass (force) pushes anyway.
	_ = m.Sync(ctx, "", false)
	if n := len(fakes[ctrl.ID].pushed); n != 1 {
		t.Errorf("unchanged sync pushed again: %d", n)
	}
	_ = m.Sync(ctx, "", true)
	if n := len(fakes[ctrl.ID].pushed); n != 2 {
		t.Errorf("forced sync pushes = %d", n)
	}

	// A ladder naming w1 goes when w1 is removed; w1 is told to drop its mesh.
	if err := st.ExitLadders.Set(ctx, "org", "o1", []string{ctrl.ID, "w1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveNode(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	rs, _ := st.ExitLadders.List(ctx, "org", "o1")
	if len(rs) != 1 || rs[0].NodeID != ctrl.ID {
		t.Errorf("ladder after removal = %+v", rs)
	}
	last := fakes["w1"].pushed[len(fakes["w1"].pushed)-1]
	if len(last.Peers) != 0 || len(last.Links) != 0 {
		t.Errorf("removed node not cleared: %+v", last)
	}
}

func TestAllocateNodeRangeKeepsForgotten(t *testing.T) {
	ctx := context.Background()
	st := storetest.Store(t)
	storetest.Controller(t, st) // 10.213.0.0/16
	m := newMeshSvc(st, func(string) (MeshNode, bool) { return nil, false })
	first, err := m.AllocateNodeRange(ctx)
	if err != nil || first.String() != "10.208.0.0/16" {
		t.Fatalf("first = %v, %v", first, err)
	}
	if err := m.ForgetRange(ctx, first.String()); err != nil {
		t.Fatal(err)
	}
	if err := m.ForgetRange(ctx, first.String()); err != nil { // twice is one entry
		t.Fatal(err)
	}
	next, err := m.AllocateNodeRange(ctx)
	if err != nil || next.String() != "10.209.0.0/16" {
		t.Errorf("after forgetting = %v, %v", next, err)
	}
}

// sees marks n's fresh handshakes with the given nodes.
func sees(n meshNodeIn, ids ...string) meshNodeIn {
	n.Fresh, n.Seen = true, map[string]bool{}
	for _, id := range ids {
		n.Seen["wg-"+id] = true
	}
	return n
}

// h2 lost its public address: only h1 still has a tunnel to it. After
// downAfter the others reach h2 through h1 (on each end the next hop's peer
// carries the far range; h2's own entry keeps its endpoints, nothing else);
// when the direct tunnel is back, the route goes.
func TestMeshRoutesThroughPeer(t *testing.T) {
	h2 := tnode("h2", 3, "203.0.113.250")
	h2.Public = nil
	in := meshIn{Nodes: []meshNodeIn{
		sees(tnode("ctl", 0, "109.148.70.232"), "h1", "m"),
		sees(tnode("h1", 2, "89.167.81.29"), "ctl", "m", "h2"),
		sees(h2, "h1"),
		sees(tnode("m", 1, "109.148.70.233"), "ctl", "h1"),
	}}
	in.Nodes[0].Public, in.Nodes[3].Public = nil, nil // both at home, behind NAT
	m := &meshSvc{down: map[[2]string]time.Time{}}
	t0 := time.Unix(1_000_000, 0)
	if r := m.routes(in, t0); len(r) != 0 {
		t.Fatalf("routed before downAfter: %v", r)
	}
	in.Next = m.routes(in, t0.Add(downAfter))
	m.next = in.Next
	want := map[[2]string]string{{"ctl", "h2"}: "h1", {"h2", "ctl"}: "h1", {"m", "h2"}: "h1", {"h2", "m"}: "h1"}
	if !maps.Equal(in.Next, want) {
		t.Fatalf("routes = %v", in.Next)
	}
	specs := buildMesh(in)
	peer := func(node, of string) mesh.Peer {
		for _, p := range specs[node].Peers {
			if p.NodeID == of {
				return p
			}
		}
		t.Fatalf("%s has no peer %s", node, of)
		return mesh.Peer{}
	}
	h2r, h2t := netip.MustParsePrefix("10.211.0.0/16"), netip.MustParsePrefix("10.207.255.4/32")
	if p := peer("ctl", "h1"); !slices.Contains(p.AllowedIPs, h2r) || !slices.Contains(p.AllowedIPs, h2t) {
		t.Fatalf("ctl's h1 peer = %v", p.AllowedIPs)
	}
	if p := peer("ctl", "h2"); len(p.AllowedIPs) != 0 {
		t.Fatalf("ctl's h2 peer still routes: %v", p.AllowedIPs)
	}
	if p := peer("h2", "h1"); !slices.Contains(p.AllowedIPs, netip.MustParsePrefix("10.208.0.0/16")) ||
		!slices.Contains(p.AllowedIPs, netip.MustParsePrefix("10.209.0.0/16")) {
		t.Fatalf("h2's h1 peer = %v", p.AllowedIPs)
	}
	// Nobody can judge ctl-h2 (stale facts): the route stays.
	in.Nodes[0].Fresh, in.Nodes[2].Fresh = false, false
	if r := m.routes(in, t0.Add(2*downAfter)); r[[2]string{"ctl", "h2"}] != "h1" {
		t.Fatalf("unknown pair lost its route: %v", r)
	}
	// The direct tunnel is back.
	in.Nodes[0] = sees(in.Nodes[0], "h1", "m", "h2")
	in.Nodes[2].Fresh = true
	if r := m.routes(in, t0.Add(3*downAfter)); r[[2]string{"ctl", "h2"}] != "" {
		t.Fatalf("still routed after recovery: %v", r)
	}
}

// A node's public addresses outlive the panel: kept when reported, read
// back by a fresh mesh (no facts yet), dropped with the node.
func TestPublicAddrsKept(t *testing.T) {
	ctx := context.Background()
	st := storetest.Store(t)
	ctrl := storetest.Controller(t, st)
	wk := ctrl
	wk.ID, wk.Name, wk.Slug, wk.Role, wk.RangeCIDR = "w1", "w1", "w1", "worker", "10.214.0.0/16"
	if err := st.Nodes.Create(ctx, wk); err != nil {
		t.Fatal(err)
	}
	fakes := map[string]*fakeMeshNode{ctrl.ID: {}, wk.ID: {}}
	m := newMeshSvc(st, func(id string) (MeshNode, bool) { f, ok := fakes[id]; return f, ok })
	if err := m.Sync(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	restarted := newMeshSvc(st, func(string) (MeshNode, bool) { return nil, false })
	if got := restarted.publicOf("w1"); len(got) != 1 || got[0].String() != "203.0.113.7" {
		t.Fatalf("after restart publicOf = %v", got)
	}
	if err := restarted.RemoveNode(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if got := restarted.publicOf("w1"); len(got) != 0 {
		t.Errorf("forgotten node still has %v", got)
	}
}
