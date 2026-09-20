package sqlite_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Point 20 moved the declared columns into repo.TileConfig and made UpdateTile
// take one. That closes the write a caller could not see fail — a state field
// set on a Tile and handed to UpdateTile no longer compiles.
//
// One mistake is still possible and neither the compiler nor a typed test for
// one column catches it: adding a field to TileConfig and forgetting it in
// UpdateTile's SQL. sqlx binds by name, so the extra field is simply ignored
// and the column silently stops persisting. This fills EVERY config field
// reflectively and round-trips it, so the next field added is covered without
// anyone remembering to cover it.
func TestTileConfigRoundTripsEveryField(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	ctx := context.Background()

	tile := *seed.Tile
	tile.ID = "cfg-1"
	tile.Slug = "cfg"
	require.NoError(t, store.CreateTile(ctx, &tile), "create")

	// Distinct non-zero value per field, so a column wired to the wrong
	// parameter shows up as a mismatch rather than coincidentally matching.
	want := repo.TileConfig{}
	v := reflect.ValueOf(&want).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("v" + v.Type().Field(i).Name)
		case reflect.Int:
			f.SetInt(int64(i + 1))
		case reflect.Float64:
			f.SetFloat(float64(i) + 0.5)
		case reflect.Bool:
			// Alternating, not all-true: two bool columns bound to each
			// other's parameter would round-trip cleanly if every bool
			// carried the same value.
			f.SetBool(i%2 == 0)
		default:
			t.Fatalf("TileConfig.%s is a %s — teach this test how to fill it",
				v.Type().Field(i).Name, f.Kind())
		}
	}
	// Env is parsed into variable rows on write, so it has to be an env blob.
	want.Env = "ROUNDTRIP=yes"

	require.NoError(t, store.UpdateTile(ctx, tile.ID, want), "update")
	got, err := store.GetTile(ctx, tile.ID)
	require.NoError(t, err, "get")
	require.NotNil(t, got, "get")

	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		assert.Equal(t, v.Field(i).Interface(),
			reflect.ValueOf(got.TileConfig).Field(i).Interface(),
			"TileConfig.%s did not survive UpdateTile — missing from its SQL?", name)
	}
}

// The other half: a config save must not touch observed state. This was always
// true in the SQL; what is new is that it is now true in the type, and this
// asserts the behaviour the type is protecting rather than the type itself.
func TestUpdateTileLeavesStateAlone(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	ctx := context.Background()

	require.NoError(t, store.UpdateTileStatus(ctx, seed.Tile.ID, "running"), "status")
	require.NoError(t, store.SetTileHomeNode(ctx, seed.Tile.ID, "node-a"), "home node")
	require.NoError(t, store.SetTileImageDigest(ctx, seed.Tile.ID, "sha256:deployed"), "digest")

	// A settings save rendered before the deploy finished: stale state on the
	// struct, fresh config. Only the config may land.
	stale, err := store.GetTile(ctx, seed.Tile.ID)
	require.NoError(t, err, "get")
	stale.Status = "idle"
	stale.HomeNode = ""
	stale.ImageDigest = ""
	stale.MemLimitMB = 777
	require.NoError(t, store.UpdateTile(ctx, stale.ID, stale.TileConfig), "update")

	got, err := store.GetTile(ctx, seed.Tile.ID)
	require.NoError(t, err, "get")
	assert.Equal(t, 777, got.MemLimitMB, "config did not land")
	assert.Equal(t, "running", got.Status, "a settings save reverted status")
	assert.Equal(t, "node-a", got.HomeNode, "a settings save cleared the home node")
	assert.Equal(t, "sha256:deployed", got.ImageDigest, "a settings save cleared the deployed digest")
}
