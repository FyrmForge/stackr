package graph

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// mkApp/mkDB build fixtures whose first 8 ID chars are unique (aliases).
func mkApp(n int, env string) repo.Tile {
	return repo.Tile{
		ID:   fmt.Sprintf("aaaa%04d-0000-0000-0000-000000000000", n),
		Name: fmt.Sprintf("app-%d", n),
		Env:  env,
	}
}

func mkDB(n, extPort int) repo.Tile {
	return repo.Tile{
		ID:           fmt.Sprintf("dddd%04d-0000-0000-0000-000000000000", n),
		Name:         fmt.Sprintf("db-%d", n),
		Slug:         fmt.Sprintf("db-%d", n),
		Engine:       "postgres",
		ExternalPort: extPort,
	}
}

// refs builds the consumer -> referenced-tile map Build now takes, replacing
// the old "does the env text mention the slug" guess.
func refs(pairs map[repo.Tile][]repo.Tile) map[string][]string {
	out := map[string][]string{}
	for consumer, targets := range pairs {
		for _, t := range targets {
			out[consumer.ID] = append(out[consumer.ID], t.ID)
		}
	}
	return out
}

// checkInvariants asserts layout properties that must hold for ANY topology:
// no overlapping cards, everything grid-ish, deterministic.
func checkInvariants(t *testing.T, g Graph) {
	t.Helper()
	const w, h = 220.0, 96.0
	for i, a := range g.Nodes {
		for j, b := range g.Nodes {
			if i >= j {
				continue
			}
			// Tucked volume cards overlap their parent by design.
			if a.Kind == KindVolume || b.Kind == KindVolume {
				continue
			}
			assert.False(t, a.X < b.X+w && b.X < a.X+w && a.Y < b.Y+h && b.Y < a.Y+h,
				"nodes overlap: %s(%v,%v) and %s(%v,%v)", a.Name, a.X, a.Y, b.Name, b.X, b.Y)
		}
	}
	for _, n := range g.Nodes {
		if n.Kind == KindVolume {
			continue // tucked offset is parent-relative, not grid-aligned
		}
		assert.Equal(t, 0, int(n.Y)%22, "node %s Y %v not on 22px grid", n.Name, n.Y)
	}
}

func TestLayoutManyComponents(t *testing.T) {
	// 3 independent app+db pairs, one shared db, one 2-db app, 2 orphans
	dbs := []repo.Tile{mkDB(1, 0), mkDB(2, 0), mkDB(3, 5432), mkDB(4, 0), mkDB(5, 0)}
	apps := []repo.Tile{mkApp(1, ""), mkApp(2, ""), mkApp(3, ""), mkApp(4, ""), mkApp(5, "")}
	r := refs(map[repo.Tile][]repo.Tile{
		apps[0]: {dbs[0]},
		apps[1]: {dbs[1]},
		apps[2]: {dbs[2], dbs[3]},
		apps[3]: {dbs[2]}, // shared db
		// apps[4] is an orphan
	})
	domains := map[string][]string{apps[0].ID: {"a.example.com"}, apps[2].ID: {"c.example.com"}}

	g := Build(apps, dbs, nil, domains, nil, nil, nil, r)
	g.Arrange(ArrangeClusters, nil)
	checkInvariants(t, g)

	// proxy + host synthetic nodes exist and are wired
	var proxyEdges, portEdges int
	for _, e := range g.Edges {
		switch e.Kind {
		case "ingress":
			proxyEdges++
		case "port":
			portEdges++
		}
	}
	require.Equal(t, 2, proxyEdges, "want 2 ingress + 1 port edges, got %d + %d", proxyEdges, portEdges)
	require.Equal(t, 1, portEdges, "want 2 ingress + 1 port edges, got %d + %d", proxyEdges, portEdges)

	// connected nodes should sit near their neighbors: every app-db edge
	// shorter (in Y) than the full layout height
	nodeByID := map[string]Node{}
	var minY, maxY float64
	for _, n := range g.Nodes {
		nodeByID[n.ID] = n
		if n.Y < minY {
			minY = n.Y
		}
		if n.Y > maxY {
			maxY = n.Y
		}
	}
	span := maxY - minY
	for _, e := range g.Edges {
		dy := nodeByID[e.From].Y - nodeByID[e.To].Y
		if dy < 0 {
			dy = -dy
		}
		if span > 0 {
			assert.LessOrEqual(t, dy, span/2+1, "edge %s->%s spans %v of %v total height; nodes not near neighbors", e.From, e.To, dy, span)
		}
	}

	// orphan app parked below every connected node
	orphan := nodeByID[AppNodeID(apps[4].ID)]
	for _, n := range g.Nodes {
		if n.ID != orphan.ID && !strings.HasPrefix(n.ID, "db:"+dbs[4].ID[:0]) && n.Y > orphan.Y {
			if n.ID == DBNodeID(dbs[4].ID) {
				continue // the other orphan may sit below
			}
			assert.Fail(t, fmt.Sprintf("connected node %s (Y %v) below orphan (Y %v)", n.Name, n.Y, orphan.Y))
		}
	}
}

// A new card whose only neighbor was hand-dragged lands beside that neighbor,
// not in its kind's column across the canvas.
func TestLayoutNewCardBesideDraggedNeighbor(t *testing.T) {
	db := mkDB(1, 0)
	app := mkApp(1, "")
	positions := map[string][2]float64{
		DBNodeID(db.ID): {1100, -88}, // hand-dragged far top-right
	}
	g := Build([]repo.Tile{app}, []repo.Tile{db}, nil, nil, positions, nil, nil,
		refs(map[repo.Tile][]repo.Tile{app: {db}}))
	g.Arrange(ArrangeClusters, positions)
	checkInvariants(t, g)
	var an Node
	for _, n := range g.Nodes {
		if n.ID == AppNodeID(app.ID) {
			an = n
		}
	}
	dx := 1100 - an.X
	assert.False(t, dx < 200 || dx > 400, "app X %v not beside neighbor at 1100", an.X)
	dy := an.Y - (-88)
	assert.False(t, dy < -50 || dy > 200, "app Y %v not near neighbor at -88", an.Y)
}

func TestLayoutDeterministicLargeGraph(t *testing.T) {
	var apps []repo.Tile
	var dbs []repo.Tile
	for i := 1; i <= 6; i++ {
		dbs = append(dbs, mkDB(i, i%2*5000))
	}
	for i := 1; i <= 8; i++ {
		apps = append(apps, mkApp(i, ""))
	}
	pairs := map[repo.Tile][]repo.Tile{}
	for i := 0; i < 6; i++ {
		pairs[apps[i]] = []repo.Tile{dbs[i]}
	}
	r := refs(pairs)
	domains := map[string][]string{apps[0].ID: {"x.dev"}, apps[3].ID: {"y.dev"}}
	g1 := Build(apps, dbs, nil, domains, nil, nil, nil, r)
	g2 := Build(apps, dbs, nil, domains, nil, nil, nil, r)
	g1.Arrange(ArrangeClusters, nil)
	g2.Arrange(ArrangeClusters, nil)
	checkInvariants(t, g1)
	for i := range g1.Nodes {
		require.Equal(t, g1.Nodes[i].X, g2.Nodes[i].X, "not deterministic at %s", g1.Nodes[i].Name)
		require.Equal(t, g1.Nodes[i].Y, g2.Nodes[i].Y, "not deterministic at %s", g1.Nodes[i].Name)
	}
}

func TestStyleFromPrefs(t *testing.T) {
	assert.Equal(t, ArrangeClusters, StyleFromPrefs(""))
	assert.Equal(t, ArrangeClusters, StyleFromPrefs("not json"))
	assert.Equal(t, ArrangeClusters, StyleFromPrefs(`{"arrange":"columns"}`))
	assert.Equal(t, ArrangeFlow, StyleFromPrefs(`{"arrange":"flow","snap":true}`))
}

// Three app+db islands all reading one shared db: the shared db is a hub in
// its own column, each island keeps its own horizontal band.
func TestClustersLayoutIslandsAndHubs(t *testing.T) {
	dbs := []repo.Tile{mkDB(1, 0), mkDB(2, 0), mkDB(3, 0), mkDB(4, 0)}
	apps := []repo.Tile{mkApp(1, ""), mkApp(2, ""), mkApp(3, "")}
	r := refs(map[repo.Tile][]repo.Tile{
		apps[0]: {dbs[0], dbs[3]},
		apps[1]: {dbs[1], dbs[3]},
		apps[2]: {dbs[2], dbs[3]},
	})
	g := Build(apps, dbs, nil, nil, nil, nil, nil, r)
	g.Arrange(ArrangeClusters, nil)
	checkInvariants(t, g)

	byID := map[string]Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	hub := byID[DBNodeID(dbs[3].ID)]
	for i := 0; i < 3; i++ {
		app, db := byID[AppNodeID(apps[i].ID)], byID[DBNodeID(dbs[i].ID)]
		// island shape: app feeds its db, so the db sits one step right
		assert.Less(t, app.X, db.X, "island %d: db not right of its app", i)
		// hub column sits left of every island
		assert.Less(t, hub.X, app.X, "hub not left of island %d", i)
	}
	// islands are bands: sort the three apps by Y, their dbs must keep the
	// same order (no interleaving of islands)
	type band struct{ app, db float64 }
	var bands []band
	for i := 0; i < 3; i++ {
		bands = append(bands, band{byID[AppNodeID(apps[i].ID)].Y, byID[DBNodeID(dbs[i].ID)].Y})
	}
	sort.Slice(bands, func(a, b int) bool { return bands[a].app < bands[b].app })
	for i := 1; i < len(bands); i++ {
		assert.Greater(t, bands[i].db, bands[i-1].db, "islands interleave: %+v", bands)
	}
}

// Flow layers by edge direction: an app that reads a db puts the db to its
// right. (This used to start from a scheduled-job card one column further
// left; scheduled jobs are gone, cron tiles are the scheduler.)
func TestFlowLayoutLayersByEdgeDirection(t *testing.T) {
	app := mkApp(1, "")
	db := mkDB(1, 0)
	g := Build([]repo.Tile{app}, []repo.Tile{db}, nil, nil, nil, nil, nil,
		refs(map[repo.Tile][]repo.Tile{app: {db}}))
	g.Arrange(ArrangeFlow, nil)
	checkInvariants(t, g)

	byID := map[string]Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	a, d := byID[AppNodeID(app.ID)], byID[DBNodeID(db.ID)]
	assert.Less(t, a.X, d.X, "app not left of the db it reads: %v / %v", a.X, d.X)
}

// A db the app reads hugs it on the right; one card-length edges.
func TestSatellitesAttachBesideTheirHost(t *testing.T) {
	apps := []repo.Tile{mkApp(1, ""), mkApp(2, "")}
	db := mkDB(1, 0)
	// app1 -> db, app1 -> app2: app1 is the busy host, db is a leaf
	g := Build(apps, []repo.Tile{db}, nil, nil, nil, nil, nil,
		refs(map[repo.Tile][]repo.Tile{apps[0]: {db}, apps[1]: {}}))
	g.Edges = append(g.Edges, Edge{From: AppNodeID(apps[0].ID), To: AppNodeID(apps[1].ID)})
	g.Arrange(ArrangeClusters, nil)
	checkInvariants(t, g)

	byID := map[string]Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	// the nudge pass may shift cards off their initial slot, so assert the
	// structure, not exact coordinates: read right of host, within a couple of
	// steps
	host := byID[AppNodeID(apps[0].ID)]
	d := byID[DBNodeID(db.ID)]
	assert.Greater(t, d.X, host.X, "db not right of its app")
	assert.InDelta(t, host.X, d.X, 2*localGap, "db drifted away from its app")
	assert.InDelta(t, host.Y, d.Y, 2*rowGap, "db drifted away from its app")
}

// Observed-traffic edges are drawn but must not drive placement: a card whose
// only tie is a conntrack flow stays in the orphan row.
func TestTrafficEdgesDoNotDriveLayout(t *testing.T) {
	apps := []repo.Tile{mkApp(1, ""), mkApp(2, "")}
	db := mkDB(1, 0)
	traffic := []TrafficPair{{From: AppNodeID(apps[0].ID), To: AppNodeID(apps[1].ID)}}
	g := Build(apps, []repo.Tile{db}, nil, nil, nil, nil, traffic,
		refs(map[repo.Tile][]repo.Tile{apps[0]: {db}}))
	g.Arrange(ArrangeClusters, nil)
	checkInvariants(t, g)

	byID := map[string]Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	// the traffic edge itself is still on the graph for the client to draw
	found := false
	for _, e := range g.Edges {
		if e.Kind == "traffic" {
			found = true
		}
	}
	require.True(t, found, "traffic edge missing from the graph: %+v", g.Edges)
	// ...but app-2 is parked below the connected pair, not pulled beside it
	pairBottom := byID[AppNodeID(apps[0].ID)].Y
	if y := byID[DBNodeID(db.ID)].Y; y > pairBottom {
		pairBottom = y
	}
	assert.Greater(t, byID[AppNodeID(apps[1].ID)].Y, pairBottom,
		"traffic-only card not in the orphan row: %+v", byID[AppNodeID(apps[1].ID)])
}

// A chain with a skip link (a -> b -> c plus a -> c) forces the skip edge
// under b when the three sit in one row; the nudge pass must clear it.
func TestNudgeClearsEdgesUnderCards(t *testing.T) {
	apps := []repo.Tile{mkApp(1, ""), mkApp(2, ""), mkApp(3, "")}
	r := refs(map[repo.Tile][]repo.Tile{
		apps[0]: {apps[1], apps[2]},
		apps[1]: {apps[2]},
	})
	for _, style := range []ArrangeStyle{ArrangeClusters, ArrangeFlow} {
		g := Build(apps, nil, nil, nil, nil, nil, nil, r)
		g.Arrange(style, nil)
		checkInvariants(t, g)

		byID := map[string]Node{}
		for _, n := range g.Nodes {
			byID[n.ID] = n
		}
		for _, e := range g.Edges {
			a, b := byID[e.From], byID[e.To]
			for _, n := range g.Nodes {
				if n.ID == e.From || n.ID == e.To {
					continue
				}
				hit := segHitsRect(a.X+CardW/2, a.Y+CardH/2, b.X+CardW/2, b.Y+CardH/2,
					n.X, n.Y, CardW, CardH)
				assert.False(t, hit, "%s: edge %s->%s passes under %s", style, e.From, e.To, n.ID)
			}
		}
	}
}
