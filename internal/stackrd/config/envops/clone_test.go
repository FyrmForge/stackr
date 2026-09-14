package envops_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// R9: a clone carries the base tile's variable definitions, secrets included.
// The env blob only holds the non-secret ones, so copying the tile row alone
// would silently drop every secret the app needs.
func TestCloneCopiesVariables(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	now := time.Now().UTC()

	for _, v := range []repo.Variable{
		{Name: "PORT", Value: "8080"},
		{Name: "TOKEN", Value: "s3cr3t", Secret: true},
		{Name: "REF", Value: "${{ stack.vars.REGION }}"},
	} {
		v.OwnerKind, v.OwnerID, v.CreatedAt, v.UpdatedAt = repo.OwnerTile, seed.Tile.ID, now, now
		require.NoError(t, s.UpsertVariable(ctx, &v), "seed var %s", v.Name)
	}

	clone := &repo.Environment{ID: "env2", StackID: seed.Stack.ID, Name: "pr-1", Slug: "pr-1",
		Type: "ephemeral", BaseEnvID: seed.Env.ID, CreatedAt: now}
	require.NoError(t, s.CreateEnvironment(ctx, clone), "create env")
	require.NoError(t, (envops.Ops{Store: s}).CloneTiles(ctx, clone), "clone")

	tiles, err := s.ListTilesByEnv(ctx, clone.ID)
	require.NoError(t, err)
	require.Len(t, tiles, 1, "cloned tiles")
	require.NotEqual(t, seed.Tile.ID, tiles[0].ID, "clone reused the base tile id")
	got, err := s.ListVariables(ctx, repo.OwnerTile, tiles[0].ID)
	require.NoError(t, err, "list")
	byName := map[string]repo.Variable{}
	for _, v := range got {
		byName[v.Name] = v
	}
	require.Len(t, byName, 3, "cloned variables: %+v", got)
	assert.Equal(t, "s3cr3t", byName["TOKEN"].Value, "secret not carried over: %+v", byName["TOKEN"])
	assert.True(t, byName["TOKEN"].Secret, "secret not carried over: %+v", byName["TOKEN"])
	// References are copied verbatim, they resolve against the clone's own
	// environment, so rewriting them here would break that.
	assert.Equal(t, "${{ stack.vars.REGION }}", byName["REF"].Value)
	// The base env is untouched.
	base, err := s.ListVariables(ctx, repo.OwnerTile, seed.Tile.ID)
	assert.NoError(t, err, "base env variables changed")
	assert.Len(t, base, 3, "base env variables changed")
}

// A clone's board starts from the base env's layout: slug-keyed rows transfer,
// uuid-keyed ones (resources/jobs/forwards point into the base env) do not.
func TestCloneCopiesLayout(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)

	base := repo.GraphOwner(repo.ScopeEnv, seed.Env.ID)
	for _, p := range []repo.NodePosition{
		{NodeID: "app:web", X: 100, Y: 200},
		{NodeID: "proxy:traefik", X: 10, Y: 20},
		{NodeID: "resource:0d9f3c9e-aaaa-bbbb-cccc-000000000001", X: 5, Y: 5},
	} {
		p.OwnerID = base
		require.NoError(t, s.UpsertNodePosition(ctx, &p), "seed position %s", p.NodeID)
	}

	clone := &repo.Environment{ID: "env2", StackID: seed.Stack.ID, Name: "pr-1", Slug: "pr-1",
		Type: "ephemeral", BaseEnvID: seed.Env.ID, CreatedAt: time.Now().UTC()}
	require.NoError(t, s.CreateEnvironment(ctx, clone), "create env")
	require.NoError(t, (envops.Ops{Store: s}).CloneTiles(ctx, clone), "clone")

	rows, err := s.ListNodePositions(ctx, repo.GraphOwner(repo.ScopeEnv, clone.ID))
	require.NoError(t, err, "list positions")
	got := map[string][2]float64{}
	for _, r := range rows {
		got[r.NodeID] = [2]float64{r.X, r.Y}
	}
	assert.Equal(t, map[string][2]float64{
		"app:web":       {100, 200},
		"proxy:traefik": {10, 20},
	}, got, "cloned layout: want app:web and proxy:traefik only")
}
