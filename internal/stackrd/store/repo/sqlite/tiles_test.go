package sqlite_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// A settings save carries whatever status the form was rendered with. If
// UpdateTile wrote it back, saving after a deploy finished would revert the
// tile to the status it had when the page was opened, the "tiles bug out"
// report. Status moves only through UpdateTileStatus.
func TestUpdateTileLeavesStatusAlone(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)

	require.NoError(t, store.UpdateTileStatus(ctx, seed.Tile.ID, "running"), "set status")
	stale, err := store.GetTile(ctx, seed.Tile.ID)
	require.NoError(t, err, "get")
	stale.Status = "idle" // what a form rendered before the deploy would send
	stale.Name = "renamed"
	require.NoError(t, store.UpdateTile(ctx, stale.ID, stale.TileConfig), "update")

	got, err := store.GetTile(ctx, seed.Tile.ID)
	require.NoError(t, err, "get after")
	assert.Equal(t, "running", got.Status, "status = %q, want running (UpdateTile stomped it)", got.Status)
	assert.Equal(t, "renamed", got.Name, "name = %q, want renamed (UpdateTile wrote nothing)", got.Name)
}

// A run outcome is not a lifecycle change: tiles.status belongs to the deploy
// (and to pause), and a failed tick must not redden the card its environment
// and stack roll up from.
func TestRecordTileRunLeavesStatusAlone(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)

	require.NoError(t, store.UpdateTileStatus(ctx, seed.Tile.ID, "paused"), "pause")
	require.NoError(t, store.RecordTileRun(ctx, seed.Tile.ID, "ok", "done\n"), "record")
	got, err := store.GetTile(ctx, seed.Tile.ID)
	require.NoError(t, err, "get")
	assert.Equal(t, "paused", got.Status, "status = %q, want paused", got.Status)
	assert.False(t, got.LastStatus != "ok" || got.LastOutput != "done\n",
		"run outcome not recorded: last_status=%q last_output=%q", got.LastStatus, got.LastOutput)

	// A failed run leaves an idle tile idle: only last_status moves.
	require.NoError(t, store.UpdateTileStatus(ctx, seed.Tile.ID, "idle"), "unpause")
	require.NoError(t, store.RecordTileRun(ctx, seed.Tile.ID, "error", "boom"), "record 2")
	got, err = store.GetTile(ctx, seed.Tile.ID)
	require.NoError(t, err, "get 2")
	assert.Equal(t, "idle", got.Status, "status = %q, want idle (a failed run reddened the card)", got.Status)
	assert.Equal(t, "error", got.LastStatus, "last_status = %q, want error", got.LastStatus)
}
