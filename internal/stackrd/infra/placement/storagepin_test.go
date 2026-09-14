package placement_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/placement"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// A local storage pool is a directory on one host, and docker creates an empty
// local volume for a name it does not know rather than failing. So a tile that
// attaches one is pinned exactly like a tile that holds a volume: let swarm
// schedule it anywhere and it comes up healthy against nothing
// (docs/plans/39-codex-review-fixes.md, point 2).
func TestLocalStorageAttachmentPinsATile(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()

	sv := &repo.Server{ID: "srv-b", Name: "worker", Kind: "swarm", NodeID: "node-b", CreatedAt: now}
	require.NoError(t, store.CreateServer(ctx, sv), "create server")
	require.NoError(t, store.CreateStorage(ctx, &repo.Storage{
		ID: "st-1", ServerID: sv.ID, Name: "Bulk", Slug: "bulk",
		Backend: "local", Export: "/srv/bulk", CreatedAt: now}), "create pool")

	tile := &repo.Tile{ID: "9c1f6f2e-1d0e-4a54-9f3e-2b6a6a7c1111",
		StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "api", Slug: "api", Kind: "app", Status: "running",
		Storage:   "bulk/data:/var/lib/data",
		CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTile(ctx, tile), "create tile")

	assert.True(t, placement.IsPinned(ctx, store, tile),
		"a local storage attachment pins the tile")
	assert.Equal(t, "node-b", placement.NodeOf(ctx, store, tile),
		"with no home node of its own it belongs on the pool's node")

	nodes, local := placement.StorageNodes(ctx, store, tile)
	assert.True(t, local, "attaches a local pool")
	assert.Equal(t, []string{"node-b"}, nodes)
}

// A network-backed pool is mountable from every node, so it pins nothing.
func TestNFSStorageDoesNotPin(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()

	require.NoError(t, store.CreateStorage(ctx, &repo.Storage{
		ID: "st-2", ServerID: "local", Name: "Shared", Slug: "shared",
		Backend: "nfs", Address: "10.0.0.9", Export: "/exports/shared", CreatedAt: now}), "create pool")

	tile := &repo.Tile{ID: "9c1f6f2e-1d0e-4a54-9f3e-2b6a6a7c2222",
		StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "web", Slug: "web", Kind: "app", Status: "running",
		Storage:   "shared/data:/var/lib/data",
		CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTile(ctx, tile), "create tile")

	assert.False(t, placement.IsPinned(ctx, store, tile), "nfs is reachable from any node")
	assert.Empty(t, placement.NodeOf(ctx, store, tile), "nothing to pin it to")
}
