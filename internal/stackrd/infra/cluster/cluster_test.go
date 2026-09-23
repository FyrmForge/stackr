package cluster_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/agent"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// An empty node used to mean "here", which is how the postgres provisioner
// passed nothing for months and ran every exec against the manager while the
// instance was on a worker. It has to be refused before anything is dialled,
// so this Cluster has no runtime behind it at all: reaching one would panic.
func TestEmptyNodeIsRefusedNotRunLocally(t *testing.T) {
	ctx := context.Background()
	c := cluster.New(nil, agent.New(nil, "", t.TempDir()), testdb.New(t))

	_, err := c.Exec(ctx, "", "cid", []string{"true"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no node given")

	_, _, err = c.ExecStream(ctx, "", "cid", []string{"true"}, nil)
	require.Error(t, err)

	err = c.CreateVolume(ctx, "", "vol")
	require.Error(t, err)

	// The guard in front of stop and remove fails closed.
	assert.True(t, c.ContainerIsSystem(ctx, "", "cid"))
}

// A pinned tile with no home node yet, and a stateless tile, both have no
// node to answer with. Saying so here, with the tile's name, beats returning
// "" and letting the empty-node rule catch it one call later.
func TestNodeOfNamesTheTileItCannotPlace(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()
	c := cluster.New(nil, agent.New(nil, "", t.TempDir()), store)

	pinned := &repo.Tile{ID: "pinned-db", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "sharedpg", Slug: "sharedpg", Kind: "database", Engine: "postgres",
		Status: "running", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTile(ctx, pinned))
	_, err := c.NodeOf(ctx, pinned)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sharedpg")
	assert.Contains(t, err.Error(), "no home node")

	_, err = c.NodeOf(ctx, seed.Tile)
	require.Error(t, err, "a stateless tile has no node of its own")
	assert.Contains(t, err.Error(), "not pinned")

	pinned.HomeNode = "node-a"
	require.NoError(t, store.UpdateTile(ctx, pinned.ID, pinned.TileConfig))
	got, err := c.NodeOf(ctx, pinned)
	require.NoError(t, err)
	assert.Equal(t, "node-a", got)
}

// A storage tile names a server, not a node, and every storage volume was
// created on the manager whatever server the operator picked: the postgres
// provisioner's bug with a different noun. The server that has not joined the
// swarm has to say so rather than quietly answering "the manager".
func TestNodeOfStorageComesFromTheServerNotTheManager(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	c := cluster.New(nil, agent.New(nil, "", t.TempDir()), store)
	now := time.Now().UTC()

	joined := &repo.Server{ID: "srv-worker", Name: "worker-2", Kind: "swarm",
		NodeID: "node-worker", CreatedAt: now}
	require.NoError(t, store.CreateServer(ctx, joined))
	pending := &repo.Server{ID: "srv-pending", Name: "new box", Kind: "swarm", CreatedAt: now}
	require.NoError(t, store.CreateServer(ctx, pending))

	got, err := c.NodeOfStorage(ctx, &repo.Storage{Slug: "nas", ServerID: joined.ID})
	require.NoError(t, err)
	assert.Equal(t, "node-worker", got, "the volume belongs on the server's own node")

	_, err = c.NodeOfStorage(ctx, &repo.Storage{Slug: "half-setup", ServerID: pending.ID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "half-setup")
	assert.Contains(t, err.Error(), "swarm")
}
