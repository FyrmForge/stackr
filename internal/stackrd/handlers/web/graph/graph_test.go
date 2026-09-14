package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func TestBuildDerivesEdgeFromEnvAlias(t *testing.T) {
	dbs := []repo.Tile{{ID: "abcd1234-0000-0000-0000-000000000000", Name: "maindb", Slug: "maindb", Engine: "postgres", Status: "running"}}
	apps := []repo.Tile{{
		ID:         "eeee5678-0000-0000-0000-000000000000",
		Name:       "web",
		SourceType: "git",
		Status:     "running",
		Env:        "DATABASE_URL=postgres://u:p@maindb:5432/maindb",
	}}

	g := Build(apps, dbs, nil, nil, nil, nil, nil, map[string][]string{apps[0].ID: {dbs[0].ID}})

	require.Len(t, g.Nodes, 2) // db (data volume rides it as a sub-tile) + app
	var refEdges []Edge
	for _, e := range g.Edges {
		if e.Kind != "volume" {
			refEdges = append(refEdges, e)
		}
	}
	require.Len(t, refEdges, 1)
	e := refEdges[0]
	require.Equal(t, AppNodeID(apps[0].ID), e.From, "edge wrong: %+v", e)
	require.Equal(t, DBNodeID(dbs[0].ID), e.To, "edge wrong: %+v", e)
}

func TestBuildNoEdgeWithoutReference(t *testing.T) {
	dbs := []repo.Tile{{ID: "abcd1234-0000-0000-0000-000000000000", Name: "maindb", Slug: "maindb", Engine: "postgres"}}
	apps := []repo.Tile{{ID: "eeee5678-0000-0000-0000-000000000000", Name: "web", Env: "FOO=bar"}}
	g := Build(apps, dbs, nil, nil, nil, nil, nil, nil)
	for _, e := range g.Edges {
		require.Equal(t, "volume", e.Kind, "want no reference edges, got %+v", e)
	}
}

func TestSavedPositionOverridesAutoLayout(t *testing.T) {
	apps := []repo.Tile{{ID: "eeee5678-0000-0000-0000-000000000000", Name: "web"}}
	pos := map[string][2]float64{AppNodeID(apps[0].ID): {12, 34}}
	g := Build(apps, nil, nil, nil, pos, nil, nil, nil)
	require.Equal(t, 12.0, g.Nodes[0].X, "saved position not applied: %+v", g.Nodes[0])
	require.Equal(t, 34.0, g.Nodes[0].Y, "saved position not applied: %+v", g.Nodes[0])
}

// The first-move jump: dragging one card used to leave its neighbours unsaved,
// so the next rebuild treated them as newcomers and re-anchored them beside the
// dragged card. The client now snapshots every position on the first drag,
// with a full snapshot a rebuild must move nothing.
func TestFullSnapshotStopsNeighbourReanchoring(t *testing.T) {
	dbs := []repo.Tile{{ID: "abcd1234-0000-0000-0000-000000000000", Name: "maindb", Slug: "maindb", Engine: "postgres"}}
	apps := []repo.Tile{{ID: "eeee5678-0000-0000-0000-000000000000", Name: "web", Env: "DATABASE_URL=postgres://u:p@maindb:5432/maindb"}}
	refs := map[string][]string{apps[0].ID: {dbs[0].ID}}

	auto := Build(apps, dbs, nil, nil, nil, nil, nil, refs)
	pos := map[string][2]float64{}
	for _, n := range auto.Nodes {
		pos[n.ID] = [2]float64{n.X, n.Y}
	}
	pos[AppNodeID(apps[0].ID)] = [2]float64{900, 900} // the drag

	g := Build(apps, dbs, nil, nil, pos, nil, nil, refs)
	for _, n := range g.Nodes {
		want, ok := pos[n.ID]
		if !ok {
			continue
		}
		require.Equal(t, want[0], n.X, "%s moved to (%v,%v), want (%v,%v)", n.ID, n.X, n.Y, want[0], want[1])
		require.Equal(t, want[1], n.Y, "%s moved to (%v,%v), want (%v,%v)", n.ID, n.X, n.Y, want[0], want[1])
		require.True(t, n.Saved, "%s has a saved position but Saved is false", n.ID)
	}

	// The anchor heuristic must still fire for a genuinely new card.
	newApp := repo.Tile{ID: "ffff0000-0000-0000-0000-000000000000", Name: "api", Env: "DATABASE_URL=postgres://u:p@maindb:5432/maindb"}
	refs[newApp.ID] = []string{dbs[0].ID}
	g2 := Build(append(apps, newApp), dbs, nil, nil, pos, nil, nil, refs)
	var added Node
	for _, n := range g2.Nodes {
		if n.ID == AppNodeID(newApp.ID) {
			added = n
		}
	}
	db := pos[DBNodeID(dbs[0].ID)]
	require.False(t, added.Saved, "new card not anchored beside its neighbour: %+v", added)
	require.Equal(t, db[1], added.Y, "new card not anchored beside its neighbour: %+v", added)
}

func TestAutoLayoutIsDeterministic(t *testing.T) {
	apps := []repo.Tile{
		{ID: "bbbb0000-0000-0000-0000-000000000000", Name: "b"},
		{ID: "aaaa0000-0000-0000-0000-000000000000", Name: "a"},
	}
	g1 := Build(apps, nil, nil, nil, nil, nil, nil, nil)
	g2 := Build(apps, nil, nil, nil, nil, nil, nil, nil)
	for i := range g1.Nodes {
		require.Equal(t, g1.Nodes[i].X, g2.Nodes[i].X, "layout not deterministic at node %d", i)
		require.Equal(t, g1.Nodes[i].Y, g2.Nodes[i].Y, "layout not deterministic at node %d", i)
	}
}

func TestAddReferences(t *testing.T) {
	apps := []repo.Tile{{ID: "eeee5678-0000-0000-0000-000000000000", Name: "web"}}
	g := Build(apps, nil, nil, nil, nil, nil, nil, nil)
	before := len(g.Nodes)
	g.AddReferences([]Reference{{
		InstanceID:  "aaaa1111-0000-0000-0000-000000000000",
		Name:        "shared-pg",
		Detail:      "postgres · shared",
		Href:        "/dbs/aaaa1111",
		ConsumerIDs: []string{apps[0].ID},
	}}, nil)

	require.Len(t, g.Nodes, before+1)
	var ref Node
	for _, n := range g.Nodes {
		if n.Kind == KindRef {
			ref = n
		}
	}
	require.Equal(t, RefNodeID("aaaa1111-0000-0000-0000-000000000000"), ref.ID, "ref node id wrong")
	// ghost parks to the right of its consumer
	var app Node
	for _, n := range g.Nodes {
		if n.ID == AppNodeID(apps[0].ID) {
			app = n
		}
	}
	assert.Greater(t, ref.X, app.X, "ref X %v not right of consumer X %v", ref.X, app.X)
	// edge consumer -> ref, kind "shared"
	var found bool
	for _, e := range g.Edges {
		if e.From == AppNodeID(apps[0].ID) && e.To == ref.ID && e.Kind == "shared" {
			found = true
		}
	}
	assert.True(t, found, "missing shared edge consumer -> ref")
}

// A provisioned slice is its own card and the instance hosting it drops off
// the canvas entirely, the slices stand in for it. Two slices on one instance
// must not collapse into one node.
func TestBuildDrawsSliceAsItsOwnNode(t *testing.T) {
	inst := repo.Tile{ID: "abcd1234-0000-0000-0000-000000000000", Name: "sharedpg", Slug: "sharedpg", Engine: "postgres"}
	app := repo.Tile{ID: "eeee5678-0000-0000-0000-000000000000", Name: "web"}
	res := []repo.ManagedResource{
		{ID: "r-orders", ProviderTileID: inst.ID, Name: "orders", Slug: "sharedpg-orders", Kind: "postgres", Status: "active"},
		{ID: "r-carts", ProviderTileID: inst.ID, Name: "carts", Slug: "sharedpg-carts", Kind: "postgres", Status: "active"},
	}
	g := Build([]repo.Tile{app}, []repo.Tile{inst}, res, nil, nil, nil, nil,
		map[string][]string{app.ID: {"r-orders", "r-carts"}})

	slices := 0
	for _, n := range g.Nodes {
		if n.Kind == KindResource {
			slices++
		}
	}
	require.Equal(t, 2, slices, "want 2 slice nodes")
	// The app talks to each slice.
	for _, want := range []string{"r-orders", "r-carts"} {
		assert.True(t, hasEdge(g, AppNodeID(app.ID), ResourceNodeID(want), ""), "missing edge app -> %s", want)
	}
	// The instance has no node of its own, card, data volume and all. Every
	// slice card carries it as the visual strip underneath instead: name,
	// scope, and a click-through to its drawer.
	for _, n := range g.Nodes {
		assert.NotEqual(t, DBNodeID(inst.ID), n.ID, "instance still rendered as a node: %s", n.ID)
		assert.NotEqual(t, "dbvol:"+inst.ID, n.ID, "instance still rendered as a node: %s", n.ID)
	}
	for _, n := range g.Nodes {
		if n.Kind != KindResource {
			continue
		}
		require.Len(t, n.Subtiles, 1, "slice %s: want 1 sub-tile, got %+v", n.Name, n.Subtiles)
		st := n.Subtiles[0]
		assert.Equal(t, "sharedpg", st.Name, "slice %s parent sub-tile wrong: %+v", n.Name, st)
		assert.Equal(t, "env-scoped", st.Detail, "slice %s parent sub-tile wrong: %+v", n.Name, st)
		assert.Equal(t, "/dbs/"+inst.ID, st.Href, "slice %s parent sub-tile wrong: %+v", n.Name, st)
		assert.Equal(t, "postgres", st.Kind, "slice %s parent sub-tile wrong: %+v", n.Name, st)
	}
	// The card wears the slice's slug, its literal tile name, the one a
	// reference uses (${{ tile.<slug>.* }}), not the raw db name, which is
	// only unique per instance and collided with same-named consumers.
	for _, n := range g.Nodes {
		if n.Kind != KindResource {
			continue
		}
		assert.Contains(t, []string{"sharedpg-orders", "sharedpg-carts"}, n.Name, "unexpected card title: %q", n.Name)
		// The subtitle is the hosting instance; what kind of slice it is lives
		// in the card's chip, and its scope on the sub-tile below.
		assert.Equal(t, "sharedpg", n.Detail, "subtitle should be the hosting instance, got %q", n.Detail)
	}
}

func hasEdge(g Graph, from, to, kind string) bool {
	for _, e := range g.Edges {
		if e.From == from && e.To == to && e.Kind == kind {
			return true
		}
	}
	return false
}

// A slice whose instance lives in another environment still hangs off that
// instance's ghost card rather than floating loose on the canvas.
func TestBuildSliceOfRemoteInstanceHangsOffGhost(t *testing.T) {
	app := repo.Tile{ID: "eeee5678-0000-0000-0000-000000000000", Name: "web"}
	remote := "aaaa1111-0000-0000-0000-000000000000" // instance in another env
	res := []repo.ManagedResource{
		{ID: "r-orders", ProviderTileID: remote, Name: "orders", Slug: "shared-orders", Kind: "postgres", Status: "active"},
	}
	g := Build([]repo.Tile{app}, nil, res, nil, nil, nil, nil,
		map[string][]string{app.ID: {"r-orders"}})

	assert.True(t, hasEdge(g, ResourceNodeID("r-orders"), RefNodeID(remote), "shared"),
		"slice not wired to its remote instance's ghost card")
	var slice Node
	for _, n := range g.Nodes {
		if n.Kind == KindResource {
			slice = n
		}
	}
	assert.NotEmpty(t, slice.Href, "slice card has no link to its instance")
}

// Object storage hands out buckets, not databases, the noun comes from the
// engine registry, and a slice with no hosting instance on this canvas falls
// back to it as the subtitle.
func TestSliceCardNamesBucketsAsBuckets(t *testing.T) {
	inst := repo.Tile{ID: "abcd1234-0000-0000-0000-000000000000", Name: "assets", Slug: "assets", Engine: "s3"}
	res := []repo.ManagedResource{
		{ID: "r-uploads", ProviderTileID: inst.ID, Name: "uploads", Slug: "assets-uploads", Kind: "s3", Status: "active"},
	}
	g := Build(nil, []repo.Tile{inst}, res, nil, nil, nil, nil, nil)
	for _, n := range g.Nodes {
		if n.Kind != KindResource {
			continue
		}
		assert.Equal(t, "assets-uploads", n.Name, "bucket card = %q / %q", n.Name, n.Detail)
		assert.Equal(t, "assets", n.Detail, "bucket card = %q / %q", n.Name, n.Detail)
	}
	assert.Equal(t, "bucket", resourceKind("s3"), "slice nouns not read from the registry")
	assert.Equal(t, "logical db", resourceKind("postgres"), "slice nouns not read from the registry")
}

// The card's icon is picked from Engine, so every db and slice card must carry
// it, a card that only says "s3" in its subtitle would draw a cylinder.
func TestCardsCarryTheirEngine(t *testing.T) {
	pg := repo.Tile{ID: "aaaa1234-0000-0000-0000-000000000000", Name: "pg", Slug: "pg", Engine: "postgres"}
	s3 := repo.Tile{ID: "bbbb1234-0000-0000-0000-000000000000", Name: "assets", Slug: "assets", Engine: "s3"}
	res := []repo.ManagedResource{
		{ID: "r-uploads", ProviderTileID: s3.ID, Name: "uploads", Slug: "assets-uploads", Kind: "s3"},
	}
	g := Build(nil, []repo.Tile{pg, s3}, res, nil, nil, nil, nil, nil)
	want := map[string]string{DBNodeID(pg.ID): "postgres", ResourceNodeID("r-uploads"): "s3"}
	for _, n := range g.Nodes {
		if e, ok := want[n.ID]; ok {
			assert.Equal(t, e, n.Engine, "%s engine", n.ID)
			delete(want, n.ID)
		}
	}
	for id := range want {
		assert.Fail(t, "no card for "+id)
	}
}

// assertSystemZoneIsProxyOnly guards the auto-layout against graph.js's drag
// clamp: workload cards may never be placed left of the system column's right
// edge, or they snap sideways the moment the user grabs them.
func assertSystemZoneIsProxyOnly(t *testing.T, g Graph) {
	t.Helper()
	// The client enforces the same wall in minWorkloadX(), off the same
	// constants, they reach it as data-* on #graph-viewport.
	limit := float64(colSystemX) + CardW + CardGapX
	for _, n := range g.Nodes {
		if n.Kind == KindProxy || n.Kind == KindHost {
			continue
		}
		require.GreaterOrEqual(t, n.X, limit, "%s (%s) auto-placed inside the system zone at x=%v", n.ID, n.Kind, n.X)
	}
}

func TestSliceHostingInstanceStillPublishesItsPort(t *testing.T) {
	// A postgres instance handing out logical databases has no card of its own,
	// but its host port is published all the same, that used to draw nothing.
	inst := repo.Tile{ID: "pg-1", Name: "pg", Kind: "database", Engine: "postgres", ExternalPort: 5432}
	res := []repo.ManagedResource{{ID: "res-1", Name: "orders", Kind: "postgres", ProviderTileID: "pg-1"}}

	g := Build(nil, []repo.Tile{inst}, res, nil, nil, nil, nil, nil)

	var host bool
	var edges []Edge
	for _, n := range g.Nodes {
		if n.ID == HostNodeID {
			host = true
		}
		require.NotEqual(t, DBNodeID("pg-1"), n.ID, "the instance should still be represented by its slices, not its own card")
	}
	for _, e := range g.Edges {
		if e.Kind == "port" {
			edges = append(edges, e)
		}
	}
	require.True(t, host, "host:ports card missing: %+v", g.Nodes)
	require.Len(t, edges, 1, "want one port edge to the slice, got %+v", edges)
	require.Equal(t, ResourceNodeID("res-1"), edges[0].To, "want one port edge to the slice, got %+v", edges)
}

func TestNewCardNeverLandsUnderAHandDraggedOne(t *testing.T) {
	// Two unconnected dbs: the column sweep would put the second exactly where
	// the user parked the first, and the new card would be invisible.
	a := repo.Tile{ID: "aaaa0000-0000-0000-0000-000000000000", Name: "aaa", Engine: "postgres"}
	b := repo.Tile{ID: "bbbb0000-0000-0000-0000-000000000000", Name: "bbb", Engine: "postgres"}
	pos := map[string][2]float64{DBNodeID(b.ID): {440, 80}} // b dragged onto the db column's first row

	g := Build(nil, []repo.Tile{a, b}, nil, nil, pos, nil, nil, nil)

	byID := map[string]Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	na, nb := byID[DBNodeID(a.ID)], byID[DBNodeID(b.ID)]
	require.Equal(t, 440.0, nb.X, "saved position moved: %+v", nb)
	require.Equal(t, 80.0, nb.Y, "saved position moved: %+v", nb)
	require.False(t, na.X == nb.X && na.Y == nb.Y, "new card placed underneath the dragged one at %v,%v", na.X, na.Y)
}

// Rolling container flows up a level: endpoints rename to their card, flows
// inside one card vanish, parallel flows sum, unmapped endpoints drop.
func TestRollupTrafficSumsAndCollapses(t *testing.T) {
	pairs := map[string]float64{
		"app:a1|db:pg":   100, // env1 -> instance card
		"app:a2|db:pg":   50,  // also env1 -> instance card: sums
		"app:a1|db:d1":   999, // both tiles in env1: collapses
		"proxy|app:a1":   10,
		"app:a1|db:gone": 5, // unmapped endpoint: dropped
		"app:a1|db:zero": 0, // zero rate: dropped
		"malformed":      7,
	}
	nodeOf := map[string]string{
		"proxy":  ProxyNodeID,
		"app:a1": EnvNodeID("env1"), "app:a2": EnvNodeID("env1"), "db:d1": EnvNodeID("env1"),
		"db:pg": DBNodeID("pg"), "db:zero": DBNodeID("zero"),
	}
	got := map[string]float64{}
	for _, p := range RollupTraffic(pairs, nodeOf) {
		got[p.From+"->"+p.To] = p.Bps
	}
	want := map[string]float64{
		EnvNodeID("env1") + "->" + DBNodeID("pg"): 150,
		ProxyNodeID + "->" + EnvNodeID("env1"):    10,
	}
	require.Len(t, got, len(want), "want %d pairs, got %v", len(want), got)
	for k, v := range want {
		assert.Equal(t, v, got[k], "%s", k)
	}
}

// A rolled-up card and the tile it stands for must not disagree about the same
// status. The renderers (nodeStatus in canvas.templ, statusSpan in graph.js)
// call both "running" and "done" Online, so a group of finished cron tiles has
// to roll up to something that draws Online too, it used to roll up to "",
// which the card renders as "idle" while drilling in still said Online.
func TestWorstStatusAgreesWithTheCardRenderers(t *testing.T) {
	tile := func(status string) repo.Tile { return repo.Tile{Status: status} }

	cases := []struct {
		name  string
		tiles []repo.Tile
		want  string
	}{
		{"all finished jobs", []repo.Tile{tile("done"), tile("done")}, "done"},
		{"a crash outranks everything", []repo.Tile{tile("running"), tile("error"), tile("building")}, "error"},
		{"building outranks running", []repo.Tile{tile("running"), tile("building")}, "building"},
		{"nothing has run yet", []repo.Tile{tile(""), tile("")}, ""},
		{"no tiles", nil, ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, WorstStatus(c.tiles), "%s", c.name)
	}

	// A volume has no lifecycle of its own; its status never becomes the
	// group's.
	vol := repo.Tile{Kind: "volume", Status: "error"}
	assert.Equal(t, "running", WorstStatus([]repo.Tile{vol, tile("running")}), "volume status leaked into the rollup")
}
