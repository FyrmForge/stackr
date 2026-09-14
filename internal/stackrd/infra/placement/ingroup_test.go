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

// Without a runtime InGroup has no way to read node labels, and it must say
// "no" rather than guess: answering yes would clear a plan's move block and
// let an apply run a pinned database against an empty directory on the wrong
// machine.
func TestInGroupWillNotGuessWithoutARuntime(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()

	inst := &repo.Tile{ID: "5d79efea-0498-430e-8ebd-f95fa1c8230d",
		StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "sharedpg", Slug: "sharedpg", Kind: "database", Engine: "postgres",
		HomeNode: "node-a", Status: "running", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTile(ctx, inst), "create instance tile")

	assert.False(t, placement.InGroup(ctx, store, nil, inst, "data"),
		"no runtime: must not clear a move block")
}
