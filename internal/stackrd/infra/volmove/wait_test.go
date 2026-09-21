package volmove

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Enqueueing a deploy only puts a row in the queue. Returning there took the
// move to done while the deployment was still pending, so a build that then
// failed left the tile stopped on both nodes with home_node already on the
// target and the modal saying Moved
// (docs/plans/39-codex-review-fixes.md, point 3).
func TestWaitForDeploymentFailsOnAnythingButDone(t *testing.T) {
	deployPoll = time.Millisecond
	t.Cleanup(func() { deployPoll = time.Second })

	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()

	tile := &repo.Tile{ID: "3f3ac0f0-0f54-4a1a-9f4b-9a9b3c8d1234",
		StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "api", Slug: "api", Kind: "app", Status: "running",
		CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTile(ctx, tile), "create tile")

	s := &Service{Store: store, Rows: storeRows{s: store}}

	for _, tc := range []struct {
		status string
		ok     bool
	}{
		{"done", true},
		{"error", false},
		{"cancelled", false},
		{"stopped", false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			d := &repo.Deployment{ID: "dep-" + tc.status, TileID: tile.ID,
				Status: tc.status, Trigger: "volume-move", CreatedAt: now}
			require.NoError(t, store.CreateDeployment(ctx, d), "create deployment")

			err := s.waitForDeployment(ctx, d.ID)
			if tc.ok {
				assert.NoError(t, err)
				return
			}
			assert.Error(t, err, "a %s deployment must fail the move", tc.status)
		})
	}
}

// A deployment that never settles must not hold the move open for ever, and a
// cancelled context has to come back out rather than spin.
func TestWaitForDeploymentGivesUpWithTheContext(t *testing.T) {
	deployPoll = time.Millisecond
	t.Cleanup(func() { deployPoll = time.Second })

	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()

	tile := &repo.Tile{ID: "3f3ac0f0-0f54-4a1a-9f4b-9a9b3c8d5678",
		StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "web", Slug: "web", Kind: "app", Status: "running",
		CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTile(ctx, tile), "create tile")
	require.NoError(t, store.CreateDeployment(ctx, &repo.Deployment{
		ID: "dep-stuck", TileID: tile.ID, Status: "running",
		Trigger: "volume-move", CreatedAt: now}), "create deployment")

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	assert.Error(t, (&Service{Store: store, Rows: storeRows{s: store}}).waitForDeployment(cctx, "dep-stuck"))
}
