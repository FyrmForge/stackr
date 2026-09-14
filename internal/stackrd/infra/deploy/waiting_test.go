package deploy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// A tile whose config names a value nobody has set is not broken, so it must
// not read as crashed: the plan deliberately let the stack be applied before
// its credentials existed. Setting the value releases it.
func TestWaitingTiles(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)

	// The resolver's unset error is what the engine keys on, wrapping and all.
	_, ok := varref.Unset(&varref.UnsetError{Scope: "stack", Name: "SESSION_SECRET"})
	require.True(t, ok)
	name, ok := varref.Unset(errWrap(&varref.UnsetError{Scope: "stack", Name: "SESSION_SECRET"}))
	require.True(t, ok, "a wrapped unset reference still has to be recognisable")
	require.Equal(t, "SESSION_SECRET", name)

	now := time.Now().UTC()
	tile := &repo.Tile{ID: "t1", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "web", Slug: "web", Kind: "service", SourceType: "image",
		Status: WaitingPrefix + "SESSION_SECRET", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTile(ctx, tile))
	require.Equal(t, "SESSION_SECRET", WaitingFor(tile.Status))
	require.Empty(t, WaitingFor("error"), "an ordinary failure is not a wait")

	ClearWaiting(ctx, store, "OTHER", seed.Stack.ID)
	after, err := store.GetTile(ctx, tile.ID)
	require.NoError(t, err)
	require.Equal(t, WaitingPrefix+"SESSION_SECRET", after.Status, "another name must not release it")

	ClearWaiting(ctx, store, "SESSION_SECRET", "some-other-stack")
	after, err = store.GetTile(ctx, tile.ID)
	require.NoError(t, err)
	require.Equal(t, WaitingPrefix+"SESSION_SECRET", after.Status, "the same name in another stack is another tenant's variable")

	ClearWaitingOrg(ctx, store, seed.Stack.OrgID, "SESSION_SECRET")
	after, err = store.GetTile(ctx, tile.ID)
	require.NoError(t, err)
	require.Equal(t, "stopped", after.Status, "setting the value leaves a tile that is simply not running")
}

func errWrap(err error) error { return &wrapped{err} }

type wrapped struct{ err error }

func (w *wrapped) Error() string { return "deploy: " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }

// A dependent of a parked tile parks on the same name, so setting the value
// once releases the whole chain. Without this it would wait out the
// depends_on deadline and go red for a stack that is merely unconfigured.
func TestDepWaiting(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)

	now := time.Now().UTC()
	mk := func(id, slug, status, deps string) *repo.Tile {
		tile := &repo.Tile{ID: id, StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
			Name: slug, Slug: slug, Kind: "service", SourceType: "image",
			Status: status, DependsOn: deps, CreatedAt: now, UpdatedAt: now}
		require.NoError(t, store.CreateTile(ctx, tile))
		return tile
	}
	mk("d1", "web", WaitingPrefix+"SMTP_PASSWORD", "")
	mk("d2", "db", "running", "")

	worker := mk("d3", "worker", "stopped", "web:healthy\ndb")
	name, ok := varref.Unset(DepWaiting(ctx, store, worker))
	require.True(t, ok, "a waiting dependency must park the dependent, not fail it")
	require.Equal(t, "SMTP_PASSWORD", name, "parks on the same name, so one Set releases both")

	healthy := mk("d4", "api", "stopped", "db:healthy")
	require.NoError(t, DepWaiting(ctx, store, healthy), "nothing waiting, nothing to park on")
}
