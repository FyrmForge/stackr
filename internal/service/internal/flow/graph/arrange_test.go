package graph

import (
	"fmt"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/canvas"
)

// tileNode is a workload card, standing in for v0's app and db fixtures.
func tileNode(id string) Node {
	return Node{
		ID:   id,
		Kind: "service",
		Name: id,
		W:    CardW,
		H:    CardH,
	}
}

// systemNode is a card behind the divider: the proxy or the internet.
func systemNode(kind string) Node {
	return Node{
		ID:     kind,
		Kind:   kind,
		Name:   kind,
		W:      CardW,
		H:      CardH,
		System: true,
	}
}

// uses is a ref edge: from reads to.
func uses(from, to string) Edge {
	return Edge{Kind: EdgeRef, From: from, To: to}
}

// envView is an env canvas of the given cards and edges, nothing saved.
func envView(nodes []Node, edges ...Edge) View {
	return View{
		Scope: canvas.Scope{Kind: canvas.Env, ID: "e"},
		Nodes: nodes,
		Edges: edges,
	}
}

// nodesByID indexes the cards, so assertions never depend on node order.
func nodesByID(v View) map[string]Node {
	m := map[string]Node{}
	for _, n := range v.Nodes {
		m[n.ID] = n
	}
	return m
}

// checkInvariants asserts what must hold for any topology: no two cards
// overlap, workloads sit right of the system column, every Y is on the grid.
func checkInvariants(t *testing.T, v View) {
	t.Helper()
	for i, a := range v.Nodes {
		for _, b := range v.Nodes[i+1:] {
			if a.X < b.X+CardW && b.X < a.X+CardW && a.Y < b.Y+CardH && b.Y < a.Y+CardH {
				t.Errorf("cards overlap: %s(%d,%d) and %s(%d,%d)", a.ID, a.X, a.Y, b.ID, b.X, b.Y)
			}
		}
	}
	w := v
	wall(&w)
	for _, n := range v.Nodes {
		if w.Walled && !n.System && n.X < w.Divider {
			t.Errorf("%s at x %d, inside the system column (divider %d)", n.ID, n.X, w.Divider)
		}
		if n.Y%Grid != 0 {
			t.Errorf("%s Y %d not on the %d px grid", n.ID, n.Y, Grid)
		}
	}
}

func TestLayoutManyComponents(t *testing.T) {
	// 3 independent app+db pairs, one shared db, one 2-db app, 2 orphans
	v := envView(
		[]Node{
			systemNode(KindProxy),
			tileNode("a1"), tileNode("a2"), tileNode("a3"), tileNode("a4"), tileNode("a5"),
			tileNode("d1"), tileNode("d2"), tileNode("d3"), tileNode("d4"), tileNode("d5"),
		},
		Edge{Kind: EdgeIngress, From: KindProxy, To: "a1"},
		Edge{Kind: EdgeIngress, From: KindProxy, To: "a3"},
		uses("a1", "d1"),
		uses("a2", "d2"),
		uses("a3", "d3"),
		uses("a3", "d4"),
		uses("a4", "d3"), // shared db
	)
	arrange(&v, nil)
	checkInvariants(t, v)

	// connected cards sit near their neighbours: every edge shorter (in Y)
	// than half the layout's height
	ns := nodesByID(v)
	minY := 0
	maxY := 0
	for _, n := range v.Nodes {
		minY = min(minY, n.Y)
		maxY = max(maxY, n.Y)
	}
	span := maxY - minY
	for _, e := range v.Edges {
		dy := ns[e.From].Y - ns[e.To].Y
		if span > 0 && max(dy, -dy) > span/2+1 {
			t.Errorf("edge %s->%s spans %d of %d total height", e.From, e.To, dy, span)
		}
	}

	// the orphan app is parked below every connected card
	orphan := ns["a5"]
	for _, n := range v.Nodes {
		if n.ID != "a5" && n.ID != "d5" && n.Y > orphan.Y {
			t.Errorf("connected card %s (Y %d) below the orphan (Y %d)", n.ID, n.Y, orphan.Y)
		}
	}
}

// A new card whose only neighbour was hand-dragged lands beside that
// neighbour, not in a column across the canvas.
func TestLayoutNewCardBesideDraggedNeighbor(t *testing.T) {
	v := envView([]Node{tileNode("a1"), tileNode("d1")}, uses("a1", "d1"))
	arrange(&v, map[string]canvas.Point{"d1": {X: 1100, Y: -88}})
	checkInvariants(t, v)
	a := nodesByID(v)["a1"]
	dx := 1100 - a.X
	if dx < 200 || dx > 400 {
		t.Errorf("app X %d not beside its neighbour at 1100", a.X)
	}
	dy := a.Y + 88
	if dy < -50 || dy > 200 {
		t.Errorf("app Y %d not near its neighbour at -88", a.Y)
	}
}

// bigGraph is a canvas with every engine path: the system column, a hub,
// islands, read and trigger satellites, a skip chain, a cycle, a saved
// card with a newcomer beside it, sub-tiles and orphans. Nodes come out of
// a map, so their order differs run to run; the edges keep theirs (v0's
// dedup keeps a pair's first direction, which is part of the graph).
func bigGraph() (View, map[string]canvas.Point) {
	cards := map[string]Node{}
	for _, id := range []string{
		"a01", "a02", "a03", "a04", "a05", "a06", "a07", "a08",
		"d01", "d02", "d03", "d04", "d05", "d06",
		"shared", "c01", "c02", "x1", "x2", "x3",
		"o1", "o2", "o3", "o4", "o5",
	} {
		cards[id] = tileNode(id)
	}
	cards[KindProxy] = systemNode(KindProxy)
	cards[KindInternet] = systemNode(KindInternet)
	a01 := cards["a01"]
	a01.Subs = []Sub{{ID: "v1", Kind: KindVolume, Name: "data"}}
	cards["a01"] = a01
	a04 := cards["a04"]
	a04.Subs = []Sub{
		{ID: "r1", Kind: KindReplica},
		{ID: "r2", Kind: KindReplica},
	}
	cards["a04"] = a04
	var nodes []Node
	for _, n := range cards {
		nodes = append(nodes, n)
	}
	edges := []Edge{
		{Kind: EdgeIngress, From: KindProxy, To: "a01"},
		{Kind: EdgeIngress, From: KindProxy, To: "a04"},
		{Kind: EdgeEgress, From: "a05", To: KindInternet},
		{Kind: EdgeStartup, From: "c01", To: "a04"},
		{Kind: EdgeStartup, From: "c02", To: "a04"},
	}
	for i := 1; i <= 6; i++ {
		edges = append(edges, uses(fmt.Sprintf("a%02d", i), fmt.Sprintf("d%02d", i)))
	}
	edges = append(edges,
		uses("a01", "shared"),
		uses("a02", "shared"),
		uses("a03", "shared"),
		uses("a06", "a07"),
		uses("a07", "a08"),
		uses("a06", "a08"),
		uses("x1", "x2"),
		uses("x2", "x3"),
		uses("x3", "x1"),
	)
	return envView(nodes, edges...), map[string]canvas.Point{"d02": {X: 1100, Y: 440}}
}

// Same graph, same layout, whatever order the nodes arrive in, and the
// layout is v0's: want is what v0's Arrange drew for this graph.
func TestLayoutDeterministicLargeGraph(t *testing.T) {
	want := map[string][2]int{
		"a01":      {374, -22},
		"a02":      {792, 440},
		"a03":      {380, 264},
		"a04":      {682, 2134},
		"a05":      {830, 1628},
		"a06":      {836, 836},
		"a07":      {1122, 836},
		"a08":      {1130, 968},
		"c01":      {682, 1848},
		"c02":      {836, 1980},
		"d01":      {682, 88},
		"d02":      {1100, 440},
		"d03":      {682, 660},
		"d04":      {990, 2134},
		"d05":      {1130, 1628},
		"d06":      {836, 1100},
		"internet": {-280, 220},
		"o1":       {80, 2442},
		"o2":       {360, 2442},
		"o3":       {640, 2442},
		"o4":       {920, 2442},
		"o5":       {80, 2574},
		"proxy":    {-280, 968},
		"shared":   {680, 264},
		"x1":       {1276, 1276},
		"x2":       {980, 1408},
		"x3":       {1280, 1408},
	}
	for run := range 20 {
		v, saved := bigGraph()
		arrange(&v, saved)
		if run == 0 {
			checkInvariants(t, v)
		}
		for _, n := range v.Nodes {
			got := [2]int{n.X, n.Y}
			if got != want[n.ID] {
				t.Fatalf("run %d: %s at %v, v0 put it at %v", run, n.ID, got, want[n.ID])
			}
		}
	}
}

// Replica subs appear only when Build reads status and Place builds
// without it: they must not move a card.
func TestLayoutReplicaSubsDoNotMove(t *testing.T) {
	build := func(replicas bool) View {
		// app reads three dbs: the third attaches under it, at its height
		host := tileNode("app")
		host.Subs = []Sub{{ID: "v1", Kind: KindVolume, Name: "data"}}
		if replicas {
			for i := range 3 {
				host.Subs = append(host.Subs, Sub{
					ID:   fmt.Sprintf("r%d", i),
					Kind: KindReplica,
				})
			}
		}
		return envView(
			[]Node{host, tileNode("d1"), tileNode("d2"), tileNode("d3"), tileNode("o1")},
			uses("app", "d1"),
			uses("app", "d2"),
			uses("app", "d3"),
		)
	}
	plain := build(false)
	arrange(&plain, nil)
	with := build(true)
	arrange(&with, nil)
	checkInvariants(t, with)
	want := nodesByID(plain)
	for _, n := range with.Nodes {
		w := want[n.ID]
		if n.X != w.X || n.Y != w.Y {
			t.Errorf("%s at %d,%d with replicas, %d,%d without", n.ID, n.X, n.Y, w.X, w.Y)
		}
	}
}

// Three app+db islands all reading one shared db: the shared db is a hub in
// its own column, each island keeps its own horizontal band.
func TestClustersLayoutIslandsAndHubs(t *testing.T) {
	v := envView(
		[]Node{
			tileNode("a1"), tileNode("a2"), tileNode("a3"),
			tileNode("d1"), tileNode("d2"), tileNode("d3"), tileNode("d4"),
		},
		uses("a1", "d1"),
		uses("a1", "d4"),
		uses("a2", "d2"),
		uses("a2", "d4"),
		uses("a3", "d3"),
		uses("a3", "d4"),
	)
	arrange(&v, nil)
	checkInvariants(t, v)
	ns := nodesByID(v)
	hub := ns["d4"]
	var bands [][2]int
	for i := 1; i <= 3; i++ {
		app := ns[fmt.Sprintf("a%d", i)]
		db := ns[fmt.Sprintf("d%d", i)]
		// island shape: the app feeds its db, so the db sits one step right
		if app.X >= db.X {
			t.Errorf("island %d: db (%d) not right of its app (%d)", i, db.X, app.X)
		}
		// the hub column sits left of every island
		if hub.X >= app.X {
			t.Errorf("hub (%d) not left of island %d (%d)", hub.X, i, app.X)
		}
		bands = append(bands, [2]int{app.Y, db.Y})
	}
	// islands are bands: sorted by app Y, their dbs keep the same order
	for i := range bands {
		for j := range bands {
			if bands[i][0] < bands[j][0] && bands[i][1] >= bands[j][1] {
				t.Errorf("islands interleave: %v", bands)
			}
		}
	}
}

// A db the app reads hugs it on the right: one card-length edges.
func TestSatellitesAttachBesideTheirHost(t *testing.T) {
	// app1 -> db, app1 -> app2: app1 is the busy host, db is a leaf
	v := envView(
		[]Node{tileNode("a1"), tileNode("a2"), tileNode("d1")},
		uses("a1", "d1"),
		uses("a1", "a2"),
	)
	arrange(&v, nil)
	checkInvariants(t, v)
	ns := nodesByID(v)
	// the nudge pass may shift cards off their first slot: assert the
	// structure, read right of its host within a couple of steps
	host := ns["a1"]
	d := ns["d1"]
	if d.X <= host.X {
		t.Errorf("db (%d) not right of its app (%d)", d.X, host.X)
	}
	if d.X-host.X > 2*localGap || max(d.Y-host.Y, host.Y-d.Y) > 2*rowGap {
		t.Errorf("db %d,%d drifted away from its app %d,%d", d.X, d.Y, host.X, host.Y)
	}
}

// Egress edges come from the last traffic sample and must not drive
// placement (v0's traffic edges): a card whose only tie is egress stays in
// the orphan row.
func TestEgressEdgesDoNotDriveLayout(t *testing.T) {
	v := envView(
		[]Node{tileNode("a1"), tileNode("a2"), tileNode("d1"), systemNode(KindInternet)},
		uses("a1", "d1"),
		Edge{Kind: EdgeEgress, From: "a2", To: KindInternet},
	)
	arrange(&v, nil)
	checkInvariants(t, v)
	ns := nodesByID(v)
	bottom := max(ns["a1"].Y, ns["d1"].Y)
	if ns["a2"].Y <= bottom {
		t.Errorf("egress-only card at Y %d, not in the orphan row below %d", ns["a2"].Y, bottom)
	}
}

// A chain with a skip link (a -> b -> c plus a -> c) forces the skip edge
// under b when the three sit in one row; the nudge pass must clear it.
func TestNudgeClearsEdgesUnderCards(t *testing.T) {
	v := envView(
		[]Node{tileNode("a1"), tileNode("a2"), tileNode("a3")},
		uses("a1", "a2"),
		uses("a1", "a3"),
		uses("a2", "a3"),
	)
	arrange(&v, nil)
	checkInvariants(t, v)
	ns := nodesByID(v)
	for _, e := range v.Edges {
		a := ns[e.From]
		b := ns[e.To]
		for _, n := range v.Nodes {
			if n.ID == e.From || n.ID == e.To {
				continue
			}
			hit := segHitsRect(
				float64(a.X+CardW/2), float64(a.Y+CardH/2),
				float64(b.X+CardW/2), float64(b.Y+CardH/2),
				float64(n.X), float64(n.Y), CardW, CardH,
			)
			if hit {
				t.Errorf("edge %s->%s passes under %s", e.From, e.To, n.ID)
			}
		}
	}
}

// Home wraps org cards four to a column, id order, stepping over a row a
// saved card holds.
func TestLayoutOrgsColumns(t *testing.T) {
	build := func() View {
		v := View{Scope: canvas.Scope{Kind: canvas.Home, ID: "u"}}
		for _, id := range []string{"org:e", "org:c", "org:a", "org:d", "org:b"} {
			v.Nodes = append(v.Nodes, card(id, KindOrg, id))
		}
		return v
	}
	v := build()
	arrange(&v, nil)
	want := map[string][2]int{
		"org:a": {0, 80},
		"org:b": {0, 220},
		"org:c": {0, 360},
		"org:d": {0, 500},
		"org:e": {360, 80},
	}
	for _, n := range v.Nodes {
		got := [2]int{n.X, n.Y}
		if got != want[n.ID] {
			t.Errorf("%s at %v, want %v", n.ID, got, want[n.ID])
		}
	}
	// org:a keeps its column slot but was dragged onto b's row: b steps down
	v = build()
	arrange(&v, map[string]canvas.Point{"org:a": {X: 10, Y: 230}})
	ns := nodesByID(v)
	b := ns["org:b"]
	if b.X != 0 || b.Y != 80 {
		t.Errorf("org:b at %d,%d, want 0,80 (the free row above the drag)", b.X, b.Y)
	}
	c := ns["org:c"]
	if c.X != 0 || c.Y != 360 {
		t.Errorf("org:c at %d,%d, want 0,360 (stepped over the saved card)", c.X, c.Y)
	}
	a := ns["org:a"]
	if !a.Saved || a.X != 10 || a.Y != 230 {
		t.Errorf("org:a = %+v, want the saved drop", a)
	}
}
