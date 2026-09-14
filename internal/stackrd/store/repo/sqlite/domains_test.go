package sqlite_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// A2: one host+path routes to exactly one tile. The unique index rejects a
// second claim, and GetDomainByHostPath is what the handlers check before insert.
func TestDomainHostPathUnique(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)

	dom := &repo.Domain{ID: "d1", TileID: seed.Tile.ID, Host: "app.example.com", Path: "/",
		ContainerPort: 8080, HTTPS: true, CreatedAt: time.Now().UTC()}
	require.NoError(t, store.CreateDomain(ctx, dom), "first create")

	// lookup finds the claim
	got, err := store.GetDomainByHostPath(ctx, "app.example.com", "/")
	require.NoError(t, err, "lookup")
	require.NotNil(t, got, "lookup want d1, got %+v", got)
	require.Equal(t, "d1", got.ID, "lookup want d1, got %+v", got)

	// free host+path is nil, no error
	free, err := store.GetDomainByHostPath(ctx, "other.example.com", "/")
	require.NoError(t, err, "free lookup want (nil,nil)")
	require.Nil(t, free, "free lookup want (nil,nil), got %+v", free)

	// duplicate claim rejected by the unique index (even a different tile row)
	dup := &repo.Domain{ID: "d2", TileID: seed.Tile.ID, Host: "app.example.com", Path: "/",
		ContainerPort: 9090, HTTPS: true, CreatedAt: time.Now().UTC()}
	require.Error(t, store.CreateDomain(ctx, dup), "duplicate host+path was accepted; unique index missing")

	// Empty path is the same claim as "/", config apply writes "" where the
	// API writes "/", and the index only sees them as one if writes normalize.
	empty := &repo.Domain{ID: "d3", TileID: seed.Tile.ID, Host: "app.example.com", Path: "",
		ContainerPort: 9090, HTTPS: true, CreatedAt: time.Now().UTC()}
	require.Error(t, store.CreateDomain(ctx, empty), `path "" bypassed the claim on path "/"`)
	got, err = store.GetDomainByHostPath(ctx, "app.example.com", "")
	require.NoError(t, err, `lookup with ""`)
	require.NotNil(t, got, `lookup with "" want d1`)
	require.Equal(t, "d1", got.ID, `lookup with "" want d1, got %+v`, got)
}

func TestDeleteOrgClearsCascadedCanvasLayouts(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)

	// One layout per scope: the org's own, the stack canvas, and the canvas of
	// one of its environments (env layouts are keyed by environment id).
	owners := []string{
		repo.GraphOwner(repo.ScopeOrg, seed.Org.ID),
		repo.GraphOwner(repo.ScopeStack, seed.Stack.ID),
		repo.GraphOwner(repo.ScopeEnv, seed.Env.ID),
	}
	for _, owner := range owners {
		require.NoError(t, s.SaveNodePositions(ctx, owner, []repo.NodePosition{{NodeID: "app:1", X: 10, Y: 20}}), "seed %s", owner)
	}

	require.NoError(t, s.DeleteOrg(ctx, seed.Org.ID), "delete org")
	for _, owner := range owners {
		rows, err := s.ListNodePositions(ctx, owner)
		require.NoError(t, err, "list %s", owner)
		assert.Len(t, rows, 0, "%s still has %d position rows after the org was deleted", owner, len(rows))
	}
}

func TestSaveNodePositionsRejectsJunk(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	owner := repo.GraphOwner(repo.ScopeEnv, "stack1")

	for name, ps := range map[string][]repo.NodePosition{
		"no node id": {{NodeID: "", X: 1, Y: 1}},
		"NaN":        {{NodeID: "app:1", X: math.NaN(), Y: 0}},
		"infinity":   {{NodeID: "app:1", X: math.Inf(1), Y: 0}},
		"off-world":  {{NodeID: "app:1", X: 1e9, Y: 0}},
	} {
		assert.Error(t, s.SaveNodePositions(ctx, owner, ps), "%s: expected an error", name)
	}
	assert.Error(t, s.SaveNodePositions(ctx, "", []repo.NodePosition{{NodeID: "app:1"}}), "empty owner: expected an error")
	// A rejected batch must write nothing at all.
	rows, _ := s.ListNodePositions(ctx, owner)
	assert.Len(t, rows, 0, "rejected batches still wrote %d rows", len(rows))
}
