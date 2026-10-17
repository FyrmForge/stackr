package service

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/exits"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
	"github.com/FyrmForge/stackr/internal/worker"
)

func tnode(id string, slot int, pub string, envs ...meshEnvIn) meshNodeIn {
	tr := netip.MustParseAddr("10.207.255." + strconv.Itoa(slot+1))
	var subs []netip.Prefix
	for _, e := range envs {
		subs = append(subs, netip.MustParsePrefix(e.Subnet))
	}
	return meshNodeIn{
		Node: mesh.Node{ID: id, PublicKey: "wg-" + id, Transit: tr, Subnets: subs, Port: MeshWGPort,
			Public: []netip.Addr{netip.MustParseAddr(pub)}},
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
	// One subject per env, its rule block fixed by its network's /24.
	if len(a.Ladders) != 3 {
		t.Fatalf("ladders = %+v", a.Ladders)
	}
	for i, l := range a.Ladders {
		if want := exitPrefBase + 2*(i+1); len(l.Subject.Sources) != 1 || l.Subject.PrefBase != want {
			t.Errorf("ladder %d = %+v, want one source at pref %d", i, l.Subject, want)
		}
	}
	l0 := a.Ladders[0] // 10.208.1.0/24 over c, then the own uplink
	// Links: one per exit node, scoped to the sources routed over it.
	if len(a.Links) != 2 {
		t.Fatalf("links = %+v", a.Links)
	}
	byIface := map[string]exits.Link{}
	for _, l := range a.Links {
		byIface[l.Iface] = l
	}
	lc := byIface[exits.Iface("c")]
	if strings.Join(lc.Sources, ",") != "10.208.1.0/24,10.208.2.0/24" || lc.PeerKey != "ex-c" || lc.Table != exitTableLink+6 {
		t.Errorf("link to c = %+v", lc)
	}
	// The exit side serves exactly those subnets, from the client's slot.
	if len(c.Serves) != 1 {
		t.Fatalf("c serves = %+v", c.Serves)
	}
	sv := c.Serves[0]
	if sv.Iface != exits.ServeIface("a") || strings.Join(sv.Subnets, ",") != "10.208.1.0/24,10.208.2.0/24" ||
		sv.PeerKey != "ex-a" || sv.Mark != exitMarkBase+1 || sv.Table != exitTableServ+1 || sv.Port != lc.Port {
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

// A node without egress gets a ladder even when it has no usable exit (the
// env's internet is blackholed, never the own uplink) and fences its env
// ranges; a node whose exit is not on the mesh yet is blackholed the same way.
func TestBuildMeshNoUplink(t *testing.T) {
	plain := tnode("p", 0, "203.0.113.1",
		meshEnvIn{Subnet: "10.208.2.0/24"},
		meshEnvIn{Subnet: "10.208.1.0/24", Ladder: []string{"x"}})
	plain.NoUplink = true
	in := meshIn{Nodes: []meshNodeIn{plain, tnode("x", 1, "203.0.113.2")}}
	p := buildMesh(in)["p"]
	if !slices.Equal(p.NoUplink, []string{"10.208.1.0/24", "10.208.2.0/24"}) {
		t.Errorf("fenced = %v", p.NoUplink)
	}
	var dead, routed *worker.LadderSpec
	for i, l := range p.Ladders {
		switch l.Subject.Sources[0] {
		case "10.208.2.0/24":
			dead = &p.Ladders[i]
		case "10.208.1.0/24":
			routed = &p.Ladders[i]
		}
	}
	if dead == nil || len(dead.Rungs) != 0 {
		t.Fatalf("no zero-rung ladder for the env without exits: %+v", p.Ladders)
	}
	if routed == nil || len(routed.Rungs) != 1 || routed.Rungs[0].NodeID != "x" {
		t.Fatalf("routed ladder = %+v", routed)
	}
	if dead.Subject.PrefBase == routed.Subject.PrefBase {
		t.Error("subjects share a rule block")
	}
	gone := tnode("g", 2, "203.0.113.3", meshEnvIn{Subnet: "10.208.3.0/24", Ladder: []string{"missing"}})
	if g := buildMesh(meshIn{Nodes: []meshNodeIn{gone}})["g"]; len(g.Ladders) != 1 || len(g.Ladders[0].Rungs) != 0 || g.NoUplink != nil {
		t.Errorf("exit not on the mesh: %+v", g)
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
	wk.RangeCIDR, wk.WGAddress, wk.WGPublicKey = "10.214.0.0/16", "10.207.255.7", "wgkey-w1"
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
	if len(rs) != 1 || rs[0].TargetID != ctrl.ID {
		t.Errorf("ladder after removal = %+v", rs)
	}
	last := fakes["w1"].pushed[len(fakes["w1"].pushed)-1]
	if len(last.Peers) != 0 || len(last.Links) != 0 {
		t.Errorf("removed node not cleared: %+v", last)
	}
}

// The pool's taken blocks include every node's reported bridges on the pool
// (shared and ingress networks have no env row): a new network is unique
// cluster-wide.
func TestPoolTakenSeesBridges(t *testing.T) {
	st := storetest.Store(t)
	storetest.Controller(t, st)
	m := newMeshSvc(st, func(string) (MeshNode, bool) { return nil, false })
	m.facts["w1"] = worker.Facts{Interfaces: []worker.Iface{
		{Name: "br-x", Prefixes: []string{"10.208.9.1/24"}},
		{Name: "eth0", Prefixes: []string{"192.168.1.5/24"}},
	}}
	got, err := m.taken(context.Background())
	if err != nil || !slices.Equal(got, []netip.Prefix{netip.MustParsePrefix("10.208.9.0/24")}) {
		t.Fatalf("taken = %v, %v", got, err)
	}
}

// Exit pairs keep their index across plans; a new pair takes the lowest free.
func TestPairIdxStable(t *testing.T) {
	p := map[[2]string]int{{"a", "c"}: 0, {"b", "c"}: 2}
	if pairIdx(p, "a", "c") != 0 || pairIdx(p, "x", "c") != 1 || pairIdx(p, "y", "c") != 3 {
		t.Fatal(p)
	}
	c, s := pairBlock(1)
	if c.String() != "10.251.0.5" || s.String() != "10.251.0.6" {
		t.Fatal(c, s)
	}
}

// sees marks n fresh, hearing the given nodes and hearing nothing from the
// rest of all (silent for rxSilent).
func sees(n meshNodeIn, all []string, ids ...string) meshNodeIn {
	n.Fresh, n.Hears = true, map[string]int{}
	for _, id := range all {
		if id != n.ID {
			n.Hears["wg-"+id] = -1
		}
	}
	for _, id := range ids {
		n.Hears["wg-"+id] = 1
	}
	return n
}

// h2 lost its public address: only h1 still has a tunnel to it. After
// downConfirm the others reach h2 through h1 (on each end the next hop's peer
// carries the far range; h2's own entry keeps its endpoints, nothing else);
// when the direct tunnel is back, the route goes.
func TestMeshRoutesThroughPeer(t *testing.T) {
	h2 := tnode("h2", 3, "203.0.113.250", meshEnvIn{Subnet: "10.208.30.0/24"})
	h2.Public = nil
	all := []string{"ctl", "h1", "h2", "m"}
	in := meshIn{Nodes: []meshNodeIn{
		sees(tnode("ctl", 0, "109.148.70.232", meshEnvIn{Subnet: "10.208.10.0/24"}), all, "h1", "m"),
		sees(tnode("h1", 2, "89.167.81.29"), all, "ctl", "m", "h2"),
		sees(h2, all, "h1"),
		sees(tnode("m", 1, "109.148.70.233", meshEnvIn{Subnet: "10.208.20.0/24"}), all, "ctl", "h1"),
	}}
	in.Nodes[0].Public, in.Nodes[3].Public = nil, nil // both at home, behind NAT
	m := &meshSvc{down: map[[2]string]time.Time{}}
	t0 := time.Unix(1_000_000, 0)
	if r := m.routes(in, t0); len(r) != 0 {
		t.Fatalf("routed before downConfirm: %v", r)
	}
	in.Next = m.routes(in, t0.Add(downConfirm))
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
	h2r, h2t := netip.MustParsePrefix("10.208.30.0/24"), netip.MustParsePrefix("10.207.255.4/32")
	if p := peer("ctl", "h1"); !slices.Contains(p.AllowedIPs, h2r) || !slices.Contains(p.AllowedIPs, h2t) {
		t.Fatalf("ctl's h1 peer = %v", p.AllowedIPs)
	}
	if p := peer("ctl", "h2"); len(p.AllowedIPs) != 0 {
		t.Fatalf("ctl's h2 peer still routes: %v", p.AllowedIPs)
	}
	if p := peer("h2", "h1"); !slices.Contains(p.AllowedIPs, netip.MustParsePrefix("10.208.10.0/24")) ||
		!slices.Contains(p.AllowedIPs, netip.MustParsePrefix("10.208.20.0/24")) {
		t.Fatalf("h2's h1 peer = %v", p.AllowedIPs)
	}
	// Nobody can judge ctl-h2 (stale facts): the route stays.
	in.Nodes[0].Fresh, in.Nodes[2].Fresh = false, false
	if r := m.routes(in, t0.Add(2*downConfirm)); r[[2]string{"ctl", "h2"}] != "h1" {
		t.Fatalf("unknown pair lost its route: %v", r)
	}
	// The direct tunnel is back.
	in.Nodes[0] = sees(in.Nodes[0], all, "h1", "m", "h2")
	in.Nodes[2].Fresh = true
	if r := m.routes(in, t0.Add(3*downConfirm)); r[[2]string{"ctl", "h2"}] != "" {
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
	wk.ID, wk.Name, wk.Slug, wk.Role, wk.RangeCIDR, wk.WGAddress = "w1", "w1", "w1", "worker", "10.214.0.0/16", "10.207.255.7"
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

// A peer's received bytes decide within a minute: moving = alive, still for
// rxSilent = dead, too little history = no verdict (keep the last state).
func TestRxVerdict(t *testing.T) {
	m := &meshSvc{}
	t0 := time.Unix(1_000_000, 0)
	at := func(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }
	m.sampleRx("a", worker.Facts{Rx: map[string]int64{"k": 100}}, at(0))
	if ev := m.rx["a"]["k"].ev(at(20)); ev != 0 {
		t.Fatalf("one sample = %d, want no verdict", ev)
	}
	m.sampleRx("a", worker.Facts{Rx: map[string]int64{"k": 150}}, at(20))
	if ev := m.rx["a"]["k"].ev(at(40)); ev != 1 {
		t.Fatalf("bytes moved = %d, want alive", ev)
	}
	m.sampleRx("a", worker.Facts{Rx: map[string]int64{"k": 150}}, at(60))
	if ev := m.rx["a"]["k"].ev(at(79)); ev != 1 {
		t.Fatalf("59 s silent = %d, want still alive", ev)
	}
	if ev := m.rx["a"]["k"].ev(at(80)); ev != -1 {
		t.Fatalf("60 s silent = %d, want dead", ev)
	}
	// A restarted controller sees a dead peer for the first time: no
	// verdict until rxSilent has passed, then dead.
	n := &meshSvc{}
	n.sampleRx("a", worker.Facts{Rx: map[string]int64{"k": 150}}, at(100))
	if ev := n.rx["a"]["k"].ev(at(120)); ev != 0 {
		t.Fatalf("fresh after restart = %d, want no verdict", ev)
	}
	if ev := n.rx["a"]["k"].ev(at(160)); ev != -1 {
		t.Fatalf("still after restart = %d, want dead", ev)
	}
}

// A restarted controller starts from the routes it saved, so its first push
// keeps a routed pair routed while no node can judge it yet.
func TestRoutesSurviveRestart(t *testing.T) {
	ctx := context.Background()
	st := storetest.Store(t)
	want := map[[2]string]string{{"ctl", "h2"}: "h1", {"h2", "ctl"}: "h1"}
	(&meshSvc{st: st}).saveRoutes(ctx, want)

	m := &meshSvc{st: st, down: map[[2]string]time.Time{}}
	m.loadRoutes(ctx)
	if !maps.Equal(m.next, want) {
		t.Fatalf("loaded = %v", m.next)
	}
	h2 := tnode("h2", 3, "192.0.2.2")
	h2.Public = nil
	in := meshIn{Nodes: []meshNodeIn{tnode("ctl", 0, "192.0.2.1"), tnode("h1", 2, "89.167.81.29"), h2}}
	in.Nodes[0].Public = nil
	// Fresh but no verdict on ctl-h2 yet (first samples after the restart).
	for i := range in.Nodes {
		in.Nodes[i].Fresh, in.Nodes[i].Hears = true, map[string]int{}
	}
	in.Nodes[1].Hears["wg-ctl"], in.Nodes[1].Hears["wg-h2"] = 1, 1
	if r := m.routes(in, time.Unix(1_000_000, 0)); r[[2]string{"ctl", "h2"}] != "h1" {
		t.Fatalf("first routes after restart = %v", r)
	}
	// A minute on, the restarted controller's own samples say ctl-h2 is
	// silent: during the confirm window the route must stay.
	in.Nodes[0].Hears["wg-h2"], in.Nodes[2].Hears["wg-ctl"] = -1, -1
	m.next = m.routes(in, time.Unix(1_000_060, 0))
	if m.next[[2]string{"ctl", "h2"}] != "h1" {
		t.Fatalf("route dropped while confirming: %v", m.next)
	}
	if r := m.routes(in, time.Unix(1_000_060, 0).Add(downConfirm)); r[[2]string{"ctl", "h2"}] != "h1" {
		t.Fatalf("route dropped after confirm: %v", r)
	}
}

// countingNode answers Facts after a pause, or not at all (hang), and
// records how many reads run at once.
type countingNode struct {
	fakeMeshNode
	hang      bool
	cur, peak *int32
}

func (c *countingNode) Facts(ctx context.Context) (worker.Facts, error) {
	n := atomic.AddInt32(c.cur, 1)
	defer atomic.AddInt32(c.cur, -1)
	for {
		p := atomic.LoadInt32(c.peak)
		if n <= p || atomic.CompareAndSwapInt32(c.peak, p, n) {
			break
		}
	}
	if c.hang {
		<-ctx.Done()
		return worker.Facts{}, ctx.Err()
	}
	time.Sleep(50 * time.Millisecond)
	return c.fakeMeshNode.Facts(ctx)
}

// Node reads run in parallel, capped, and two hanging nodes cost the cycle
// one timeout, not one each.
func TestRefreshAllParallelCapped(t *testing.T) {
	st := storetest.Store(t)
	old := refreshTimeout
	refreshTimeout = 300 * time.Millisecond
	t.Cleanup(func() { refreshTimeout = old })

	var cur, peak int32
	fakes := map[string]MeshNode{}
	var ns []store.Node
	for i := range 40 {
		id := "w" + strconv.Itoa(i)
		ns = append(ns, store.Node{ID: id, Status: "online"})
		fakes[id] = &countingNode{hang: i < 2, cur: &cur, peak: &peak}
	}
	m := newMeshSvc(st, func(id string) (MeshNode, bool) { f, ok := fakes[id]; return f, ok })

	start := time.Now()
	m.refreshAll(context.Background(), ns)
	took := time.Since(start)
	if p := atomic.LoadInt32(&peak); p > refreshCap || p < 2 {
		t.Errorf("peak concurrent reads = %d, want 2..%d", p, refreshCap)
	}
	// In line: 38 x 50 ms plus two timeouts, over 2 s. In parallel: about
	// one timeout.
	if took > 2*refreshTimeout {
		t.Errorf("cycle took %v with two hung nodes", took)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.facts["w0"]; ok {
		t.Error("hung node got facts")
	}
	if _, ok := m.facts["w39"]; !ok {
		t.Error("answering node has no facts")
	}
}

// An env's network moves with it: after a move its /24 rides the new node's
// peer entry on every other node, the old node's entry no longer carries it.
func TestMeshSubnetFollowsMove(t *testing.T) {
	env := meshEnvIn{Subnet: "10.208.7.0/24"}
	before := meshIn{Nodes: []meshNodeIn{tnode("ctl", 0, "203.0.113.1"), tnode("a", 1, "203.0.113.2", env), tnode("b", 2, "203.0.113.3")}}
	after := meshIn{Nodes: []meshNodeIn{tnode("ctl", 0, "203.0.113.1"), tnode("a", 1, "203.0.113.2"), tnode("b", 2, "203.0.113.3", env)}}
	sub := netip.MustParsePrefix(env.Subnet)
	carries := func(in meshIn, of string) bool {
		for _, p := range buildMesh(in)["ctl"].Peers {
			if p.NodeID == of {
				return slices.Contains(p.AllowedIPs, sub)
			}
		}
		return false
	}
	if !carries(before, "a") || carries(before, "b") {
		t.Fatal("before the move the subnet is not a's alone")
	}
	if carries(after, "a") || !carries(after, "b") {
		t.Fatal("after the move the subnet did not follow to b")
	}
}

// Trust roots and controller serials ride every worker's spec and never the
// controller's own; without a Trust hook both stay empty (today's behaviour).
func TestMeshSpecCarriesTrust(t *testing.T) {
	ctx := context.Background()
	st := storetest.Store(t)
	ctrl := storetest.Controller(t, st)
	wk := ctrl
	wk.ID, wk.Name, wk.Slug, wk.Role = "w1", "w1", "w1", "worker"
	wk.RangeCIDR, wk.WGAddress, wk.WGPublicKey = "10.214.0.0/16", "10.207.255.7", "wgkey-w1"
	if err := st.Nodes.Create(ctx, wk); err != nil {
		t.Fatal(err)
	}
	m := newMeshSvc(st, func(string) (MeshNode, bool) { return nil, false })
	m.ctlID = ctrl.ID
	if _, specs, err := m.plan(ctx); err != nil || len(specs["w1"].TrustRoots) != 0 || len(specs["w1"].ControllerSerials) != 0 {
		t.Fatalf("no hook: %+v %v", specs["w1"], err)
	}
	m.Trust = func(context.Context) ([]string, []string) { return []string{"root-pem"}, []string{"ab12"} }
	_, specs, err := m.plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if w := specs["w1"]; len(w.TrustRoots) != 1 || w.TrustRoots[0] != "root-pem" || len(w.ControllerSerials) != 1 || w.ControllerSerials[0] != "ab12" {
		t.Errorf("worker spec = %+v", w)
	}
	if c := specs[ctrl.ID]; len(c.TrustRoots) != 0 || len(c.ControllerSerials) != 0 {
		t.Errorf("controller spec carries trust: %+v", c)
	}
}

// Removing a node whose box is already gone (uninstalled, the remove job's
// last step) succeeds: the sync after the row goes leaves it out.
func TestMeshRemoveNodeOfAGoneBox(t *testing.T) {
	ctx := context.Background()
	st := storetest.Store(t)
	ctrl := storetest.Controller(t, st)
	wk := ctrl
	wk.ID, wk.Name, wk.Slug, wk.Role = "w1", "w1", "w1", "worker"
	wk.RangeCIDR, wk.WGAddress, wk.WGPublicKey = "10.214.0.0/16", "10.207.255.7", "wgkey-w1"
	if err := st.Nodes.Create(ctx, wk); err != nil {
		t.Fatal(err)
	}
	fakes := map[string]*fakeMeshNode{ctrl.ID: {}, wk.ID: {fail: errors.New("connection refused")}}
	m := newMeshSvc(st, func(id string) (MeshNode, bool) { f, ok := fakes[id]; return f, ok })
	if err := m.RemoveNode(ctx, "w1"); err != nil {
		t.Fatalf("remove of a gone box: %v", err)
	}
}

// An env's exit follows it when it moves: the old node drops the env's ladder
// and its link to the exit, the exit serves the new node, and an env that
// lands on its exit leaves by that node's own uplink. Pushing the new node
// alone (Ensure, before its containers start) also pushes the exit side. A
// second env on the old node keeps its rule block through all of it.
func TestMeshExitFollowsMove(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	w1 := mustNode(t, n, "w1")
	w1.Tags, w1.WGAddress, w1.WGPublicKey = `["egress"]`, "10.207.255.2", "wg-w1"
	must(t, n.st.Nodes.Update(ctx, w1))
	must(t, n.st.Nodes.Create(ctx, store.Node{ID: "h4", Name: "h4", Slug: "h4", Role: "worker", Tags: `["egress"]`,
		RangeCIDR: "10.210.0.0/16", WGAddress: "10.207.255.3", WGPublicKey: "wg-h4", Status: "online", CreatedAt: time.Now()}))
	edge := addPlacement(t, n, "edge", "h4", true)
	other := addPlacement(t, n, "w1p", "w1", true)
	ctl := n.ctl.ID
	if n.ctl.WGPublicKey == "" {
		n.ctl.WGPublicKey = "wg-ctl"
		must(t, n.st.Nodes.Update(ctx, n.ctl))
	}

	lab, err := n.orch.CreateEnv(ctx, n.stack, "lab", EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"})
	must(t, err)
	// networks come with the first deploy; dev's sorts before lab's
	for id, sub := range map[string]string{n.dev.ID: "10.208.5.0/24", lab.ID: "10.208.9.0/24"} {
		_, err := n.st.DB().ExecContext(ctx, `UPDATE environments SET subnet = ? WHERE id = ?`, sub, id)
		must(t, err)
	}
	dev, err := n.st.Environments.Get(ctx, n.dev.ID)
	must(t, err)
	lab, err = n.st.Environments.Get(ctx, lab.ID)
	must(t, err)
	if lab.NodeID != ctl || dev.Subnet == "" || lab.Subnet == "" {
		t.Fatalf("fixture: lab on %q, subnets %q %q", lab.NodeID, dev.Subnet, lab.Subnet)
	}
	must(t, n.st.ExitLadders.Set(ctx, "env", dev.ID, []string{edge.ID}))
	must(t, n.st.ExitLadders.Set(ctx, "env", lab.ID, []string{other.ID}))

	fakes := map[string]*fakeMeshNode{ctl: {}, "w1": {}, "h4": {}}
	m := newMeshSvc(n.st, func(id string) (MeshNode, bool) { f, ok := fakes[id]; return f, ok })
	m.ctlID = ctl
	for id := range fakes {
		m.exitPK[id] = "ex-" + id
	}
	spec := func(id string) worker.MeshSpec {
		t.Helper()
		s, ok := m.Spec(id)
		if !ok {
			t.Fatalf("no spec for %s", id)
		}
		return s
	}
	ladder := func(s worker.MeshSpec, sub string) *worker.LadderSpec {
		for i, l := range s.Ladders {
			if slices.Contains(l.Subject.Sources, sub) {
				return &s.Ladders[i]
			}
		}
		return nil
	}
	link := func(s worker.MeshSpec, to string) *exits.Link {
		for i, l := range s.Links {
			if l.Iface == exits.Iface(to) {
				return &s.Links[i]
			}
		}
		return nil
	}
	serve := func(s worker.MeshSpec, client string) *exits.Serve {
		for i, v := range s.Serves {
			if v.Iface == exits.ServeIface(client) {
				return &s.Serves[i]
			}
		}
		return nil
	}

	must(t, m.Sync(ctx, "", false))
	labL := ladder(spec(ctl), lab.Subnet)
	if labL == nil {
		t.Fatal("lab has no ladder")
	}
	labPref := labL.Subject.PrefBase
	l, lk, sv := ladder(spec(ctl), dev.Subnet), link(spec(ctl), "h4"), serve(spec("h4"), ctl)
	if l == nil || len(l.Rungs) != 1 || l.Rungs[0].NodeID != "h4" || lk == nil || !slices.Equal(lk.Sources, []string{dev.Subnet}) ||
		sv == nil || !slices.Equal(sv.Subnets, []string{dev.Subnet}) || l.Rungs[0].Target != strings.Split(sv.Addr, "/")[0] {
		t.Fatalf("before the move: ladder %+v link %+v serve %+v", l, lk, sv)
	}
	oldAddr := lk.Addr

	// dev moves to w1. Ensure pushes w1 and the exit that serves it.
	must(t, n.st.Environments.SetNode(ctx, dev.ID, "w1"))
	h4Pushes := len(fakes["h4"].pushed)
	must(t, m.Sync(ctx, "w1", false))
	if len(fakes["h4"].pushed) != h4Pushes+1 {
		t.Fatal("pushing the moved env's new node left its exit without the serve")
	}
	if got := fakes["h4"].pushed[len(fakes["h4"].pushed)-1]; serve(got, "w1") == nil {
		t.Fatalf("exit pushed without a serve for w1: %+v", got.Serves)
	}
	must(t, m.Sync(ctx, "", false))
	c, w, x := spec(ctl), spec("w1"), spec("h4")
	if ladder(c, dev.Subnet) != nil || link(c, "h4") != nil || serve(x, ctl) != nil {
		t.Errorf("the old node still routes dev: ladder %+v link %+v serve %+v", ladder(c, dev.Subnet), link(c, "h4"), serve(x, ctl))
	}
	l, lk, sv = ladder(w, dev.Subnet), link(w, "h4"), serve(x, "w1")
	if l == nil || lk == nil || sv == nil || !slices.Equal(lk.Sources, []string{dev.Subnet}) || !slices.Equal(sv.Subnets, []string{dev.Subnet}) ||
		l.Rungs[0].Target != strings.Split(sv.Addr, "/")[0] || lk.Addr == oldAddr || lk.Port != sv.Port {
		t.Fatalf("after the move: ladder %+v link %+v serve %+v", l, lk, sv)
	}
	if got := ladder(c, lab.Subnet); got == nil || got.Subject.PrefBase != labPref {
		t.Errorf("lab's rule block moved: %+v, was %d", got, labPref)
	}

	// dev moves onto its exit: the own uplink, nothing routed anywhere.
	must(t, n.st.Environments.SetNode(ctx, dev.ID, "h4"))
	must(t, m.Sync(ctx, "", false))
	w, x = spec("w1"), spec("h4")
	if ladder(x, dev.Subnet) != nil || len(x.Links) != 0 || len(x.Serves) != 0 || link(w, "h4") != nil || ladder(w, dev.Subnet) != nil {
		t.Errorf("on its exit: h4 %+v, w1 links %+v", x, w.Links)
	}
}

type ladderNode struct {
	fakeMeshNode
	info worker.MeshInfo
}

func (l *ladderNode) MeshInfo(context.Context) (worker.MeshInfo, error) { return l.info, nil }

// An env on the controller whose every exit is down says so, and why, with
// the exit named: the controller's own ladder state comes from the mesh's
// in-process node, as a worker's comes from its client.
func TestExitGetDownOnController(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	must(t, n.st.Nodes.Create(ctx, store.Node{ID: "node-h4", Name: "h4", Slug: "h4", Role: "worker", Tags: `["egress"]`,
		RangeCIDR: "10.210.0.0/16", WGAddress: "10.207.255.3", Status: "online", CreatedAt: time.Now()}))
	edge := addPlacement(t, n, "edge", "node-h4", true)
	_, err := n.st.DB().ExecContext(ctx, `UPDATE environments SET subnet = '10.208.5.0/24' WHERE id = ?`, n.dev.ID)
	must(t, err)
	must(t, n.st.ExitLadders.Set(ctx, "env", n.dev.ID, []string{edge.ID}))
	ctl := &ladderNode{info: worker.MeshInfo{Ladders: []worker.LadderStatus{{Sources: []string{"10.208.5.0/24"},
		State: exits.State{Active: -1, Down: true, Reason: "no exit answers (node-h4: timeout)", Since: time.Now()}}}}}
	n.orch.mesh = newMeshSvc(n.st, func(id string) (MeshNode, bool) { return ctl, id == n.ctl.ID })

	out, err := n.orch.ExitGet(ctx, ScopeEnv, n.dev.ID)
	must(t, err)
	if !out.NoInternet || out.Why != "no exit answers (h4: timeout)" || len(out.Exits) != 1 || out.Exits[0].State != RungDown {
		t.Errorf("exit view = %+v", out)
	}
}

// Two nodes that are each other's exit keep the link and the serve on
// separate interfaces.
func TestBuildMeshMutualExits(t *testing.T) {
	in := meshIn{Nodes: []meshNodeIn{
		tnode("a", 0, "203.0.113.1", meshEnvIn{Subnet: "10.208.1.0/24", Ladder: []string{"b"}}),
		tnode("b", 1, "203.0.113.2", meshEnvIn{Subnet: "10.208.2.0/24", Ladder: []string{"a"}}),
	}}
	for id, s := range buildMesh(in) {
		if len(s.Links) != 1 || len(s.Serves) != 1 || s.Links[0].Iface == s.Serves[0].Iface {
			t.Errorf("%s: links %+v serves %+v", id, s.Links, s.Serves)
		}
	}
}
