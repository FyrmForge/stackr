package project

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/graph"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// The stack canvas shows envs, not containers, so the traffic sampler's
// per-container endpoints have to be renamed to whichever card stands for them
// here before graph.RollupTraffic can group them. Getting this map wrong is
// silent (a lane lands on the wrong edge, or none appears) so it is asserted
// directly rather than through the rendered graph.
func TestStackGraphMapsTrafficEndpointsToCards(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	now := time.Now().UTC()

	// two instances in the env: a stack-scoped one (its own card here) and an
	// env-scoped one (lives inside the env card)
	shared := &repo.Tile{ID: "pg-shared", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "shared", Slug: "shared", Kind: "service", Engine: "postgres",
		ScopeKind: "stack", ScopeID: seed.Stack.ID,
		Status: "running", CreatedAt: now, UpdatedAt: now}
	local := &repo.Tile{ID: "pg-local", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "local", Slug: "local", Kind: "service", Engine: "postgres",
		Status: "running", CreatedAt: now, UpdatedAt: now}
	for _, tl := range []*repo.Tile{shared, local} {
		require.NoError(t, s.CreateTile(ctx, tl), "create tile %s", tl.ID)
	}
	slices := []struct{ id, provider string }{
		{"res-shared", shared.ID},
		{"res-a", local.ID},
		{"res-b", local.ID},
	}
	for _, r := range slices {
		require.NoError(t, s.CreateResource(ctx, &repo.ManagedResource{ID: r.id, EnvironmentID: seed.Env.ID,
			ProviderTileID: r.provider, Name: r.id, Slug: r.id, Kind: "postgres",
			Status: "active", CreatedAt: now, UpdatedAt: now}), "create resource %s", r.id)
	}

	h := &handler{store: s, envs: service.NewEnvironmentService(s, nil, nil, nil, service.NewGateService(s)), vars: service.NewVariableService(s, nil, nil, nil), stacks: service.NewStackService(s, nil, nil, service.NewEnvironmentService(s, nil, nil, nil, service.NewGateService(s)), nil, service.NewGateService(s), nil), tiles: service.NewTileService(s, nil, nil, nil, nil, nil, service.NewGateService(s)), orgs: service.NewOrgService(s)}
	_, nodeOf, err := h.buildStackGraph(ctx, seed.Stack, graph.ArrangeClusters)
	require.NoError(t, err, "buildStackGraph")

	// Both sampler spellings of a tile are claimed (see mapTile), so every tile
	// contributes two entries.
	env := graph.EnvNodeID(seed.Env.Slug)
	want := map[string]string{
		"proxy": graph.ProxyNodeID,
		// a plain service tile is represented by the env card holding it
		"app:" + seed.Tile.ID: env,
		"db:" + seed.Tile.ID:  env,
		// stack-scoped instance: its own card on this canvas owns its lane
		"app:" + shared.ID: graph.DBNodeID(shared.ID),
		"db:" + shared.ID:  graph.DBNodeID(shared.ID),
		// env-scoped instance: lives inside the env card, like any tile
		"app:" + local.ID: env,
		"db:" + local.ID:  env,
	}
	for k, v := range want {
		assert.Equal(t, v, nodeOf[k], "nodeOf[%q]", k)
	}
	assert.Len(t, nodeOf, len(want), "nodeOf entries: %v", nodeOf)
}

// The env canvas used to fold traffic with its own loop, which kept a flow
// whose ends land on the same card, a tile talking to itself drew a lane
// looping back into itself here and nowhere else. It shares RollupTraffic
// with the stack and org canvases now, so all three answer alike.
func TestEnvTrafficDropsSelfFlowsAndForeignEndpoints(t *testing.T) {
	tiles := []repo.Tile{
		{ID: "t-web", Kind: "service"},
		{ID: "t-db", Kind: "service", Engine: "postgres"},
	}
	pairs := map[string]float64{
		"app:t-web|db:t-db":   1000, // real lane
		"app:t-web|app:t-web": 500,  // a tile talking to itself: not a line
		"proxy|app:t-web":     20,   // the proxy card
		"app:t-web|app:other": 900,  // endpoint in another env
		"app:t-web|db:t-db#2": 0,    // no bytes moving
	}

	got := map[string]float64{}
	for _, p := range graph.RollupTraffic(pairs, envNodeOf(tiles)) {
		got[p.From+"->"+p.To] = p.Bps
	}
	want := map[string]float64{
		graph.AppNodeID("t-web") + "->" + graph.DBNodeID("t-db"): 1000,
		graph.ProxyNodeID + "->" + graph.AppNodeID("t-web"):      20,
	}
	require.Len(t, got, len(want), "want %d lanes, got %v", len(want), got)
	for k, v := range want {
		assert.Equal(t, v, got[k], "%s", k)
	}
}
