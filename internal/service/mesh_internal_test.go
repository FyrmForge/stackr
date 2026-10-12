package service

import (
	"context"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"

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

// Two NAT'd homes on different LANs get a relay on the public node, last in
// each other's candidates; a home and the public node connect directly.
func TestBuildMeshAutoRelay(t *testing.T) {
	home := func(id string, slot int, lan, nat string) meshNodeIn {
		n := tnode(id, slot, "203.0.113.250")
		n.Public = nil
		n.Ifaces = []netip.Prefix{netip.MustParsePrefix(lan)}
		n.NAT = netip.MustParseAddr(nat)
		return n
	}
	in := meshIn{Nodes: []meshNodeIn{
		home("a", 0, "192.168.1.10/24", "109.148.70.232"),
		home("b", 1, "192.168.5.10/24", "81.2.69.160"),
		tnode("h", 2, "89.167.81.29"),
	}}
	specs := buildMesh(in)
	relayAt := netip.MustParseAddrPort("89.167.81.29:52301")
	if got := specs["h"].Relays; len(got) != 1 || got[0].Port != 52301 ||
		got[0].A != netip.MustParseAddr("109.148.70.232") || got[0].B != netip.MustParseAddr("81.2.69.160") {
		t.Fatalf("relays on h = %+v", got)
	}
	for _, p := range specs["a"].Peers {
		switch p.NodeID {
		case "b":
			if len(p.Candidates) != 1 || p.Candidates[0] != relayAt {
				t.Fatalf("a->b = %v", p.Candidates)
			}
		case "h":
			if len(p.Candidates) != 1 || p.Candidates[0] != netip.MustParseAddrPort("89.167.81.29:51820") {
				t.Fatalf("a->h = %v", p.Candidates)
			}
		}
	}
	// Same LAN: no relay.
	in.Nodes[1] = home("b", 1, "192.168.1.11/24", "81.2.69.160")
	if r := autoRelays(in); len(r) != 0 {
		t.Fatalf("same LAN relays = %+v", r)
	}
	// NAT not seen yet: no relay.
	in.Nodes[1] = home("b", 1, "192.168.5.10/24", "81.2.69.160")
	in.Nodes[1].NAT = netip.Addr{}
	if r := autoRelays(in); len(r) != 0 {
		t.Fatalf("unknown NAT relays = %+v", r)
	}
}

// A public node's wg endpoint for a NAT'd node is that node's NAT address.
func TestLearnNAT(t *testing.T) {
	m := &meshSvc{facts: map[string]worker.Facts{
		"h": {Seen: map[string]string{"wg-a": "109.148.70.232:1459", "wg-b": "10.0.0.2:51820"}},
	}}
	in := meshIn{Nodes: []meshNodeIn{tnode("h", 2, "89.167.81.29"), tnode("a", 0, "203.0.113.1"), tnode("b", 1, "203.0.113.2")}}
	in.Nodes[1].Public, in.Nodes[2].Public = nil, nil
	m.learnNAT(&in)
	if in.Nodes[1].NAT != netip.MustParseAddr("109.148.70.232") || in.Nodes[2].NAT.IsValid() {
		t.Fatalf("nat a=%v b=%v", in.Nodes[1].NAT, in.Nodes[2].NAT)
	}
}
