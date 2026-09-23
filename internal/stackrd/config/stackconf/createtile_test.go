package stackconf

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// createTile had NO test. Measured 2026-09-20: `go tool cover` put it at 0.0%,
// and a panic planted in its body did not fire once across all 220 tests in
// this package. Every apply test seeds the tile it then updates, so the create
// branch of the walk was never taken.
//
// That is what let the three `if a.Ops.Tiles != nil` fallbacks sit here
// looking like they protected tests. They protected nothing — no test reached
// them either way. The guards are gone; these are the tests that make their
// absence mean something.
//
// What matters about this path is that it runs the SAME validator the panel
// and the API run. A file that declares a tile those two would refuse must be
// refused here, at apply, rather than at deploy time as a failed container
// with the plan already marked applied.

// tileApplier is an Applier wired the way cmd/stackrd wires it: with the tile
// service. The infra below the service (cluster, proxy, scheduler, engine) is
// nil, which those all tolerate — what is under test is the rule, not the
// container.
func tileApplier(store repo.Store) Applier { return applier(Planner{Store: store}) }

func TestConfigApplyCreatesATile(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)

	r, err := Load([]byte(`version: 1
stack: Stack
environments:
  prod:
    tiles:
      app:
        type: service
        image: nginx:1
      worker:
        type: service
        image: busybox:1
`), nil)
	require.NoError(t, err)

	ok, err := tileApplier(store).ApplyResolved(ctx, seed.Stack, r, DiffOpts{}, true)
	require.NoError(t, err, "apply")
	require.True(t, ok)

	tiles, err := store.ListTilesByEnv(ctx, seed.Env.ID)
	require.NoError(t, err)
	var slugs []string
	for i := range tiles {
		slugs = append(slugs, tiles[i].Slug)
	}
	assert.ElementsMatch(t, []string{"app", "worker"}, slugs, "the file's new tile was created")
}

// The shared validator, on the create path. A negative limit is refused by
// TileService, and only by TileService: the file schema does not check signs,
// and before the service was wired in here a config apply wrote the row and
// the refusal arrived as a failed container.
//
// (The first rule tried for this test, a cron carrying a port, turned out to
// be caught earlier by the file loader — so it proved the schema, not the
// service. This one reaches Validate.)
func TestConfigApplyRefusesWhatTheOtherSurfacesRefuse(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)

	r, err := Load([]byte(`version: 1
stack: Stack
environments:
  prod:
    tiles:
      app:
        type: service
        image: nginx:1
      greedy:
        type: service
        image: busybox:1
        limits:
          cpu: -1
`), nil)
	require.NoError(t, err)

	_, err = tileApplier(store).ApplyResolved(ctx, seed.Stack, r, DiffOpts{}, true)
	require.Error(t, err, "a negative cpu limit is refused on every other surface")
	assert.Contains(t, err.Error(), "cpu limit must not be negative")

	tiles, err := store.ListTilesByEnv(ctx, seed.Env.ID)
	require.NoError(t, err)
	for i := range tiles {
		assert.NotEqual(t, "greedy", tiles[i].Slug, "the refused tile must not exist")
	}
}

// deleteTile was the third 0.0% function, and the third fallback. Dropping a
// tile from the file has to run the service's teardown, not a bare row
// delete: the row cascades cron_jobs and backups, and only the service
// re-registers those tables afterwards — without it a removed cron kept
// ticking until the next restart.
func TestConfigApplyDeletesATileThroughTheService(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)

	// The seed's "app" tile is not in this file, so the apply drops it.
	r, err := Load([]byte(`version: 1
stack: Stack
environments:
  prod:
    tiles:
      keeper:
        type: service
        image: nginx:1
`), nil)
	require.NoError(t, err)

	// force: a delete is the change the unattended path holds for a human.
	_, err = tileApplier(store).ApplyResolved(ctx, seed.Stack, r, DiffOpts{}, true)
	require.NoError(t, err, "apply")

	tiles, err := store.ListTilesByEnv(ctx, seed.Env.ID)
	require.NoError(t, err)
	var slugs []string
	for i := range tiles {
		slugs = append(slugs, tiles[i].Slug)
	}
	assert.ElementsMatch(t, []string{"keeper"}, slugs, "the tile the file dropped is gone")
}
