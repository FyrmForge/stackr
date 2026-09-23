package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// R1: a variable set round-trips through encryption unchanged (including a
// quoted multiline value), and a second write of the same name overwrites
// instead of accumulating a suffixed twin the way the old env blob did.
func TestVariableUpsertRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()

	want := map[string]string{
		"PORT":     "8080",
		"KEY":      "-----BEGIN KEY-----\nline two\n  indented\n-----END KEY-----",
		"WITH_EQ":  "a=b=c",
		"REF":      "${{ tile.db.DATABASE_URL }}",
		"EMPTYISH": "",
	}
	for name, val := range want {
		v := &repo.Variable{OwnerKind: repo.OwnerTile, OwnerID: seed.Tile.ID, Name: name,
			Value: val, Secret: name == "KEY", CreatedAt: now, UpdatedAt: now}
		require.NoError(t, store.UpsertVariable(ctx, v), "upsert %s", name)
	}

	got, err := store.ListVariables(ctx, repo.OwnerTile, seed.Tile.ID)
	require.NoError(t, err, "list")
	require.Len(t, got, len(want))
	for _, v := range got {
		assert.Equal(t, want[v.Name], v.Value, "%s", v.Name)
	}

	// Same name again: overwrite, not a second row.
	require.NoError(t, store.UpsertVariable(ctx, &repo.Variable{OwnerKind: repo.OwnerTile, OwnerID: seed.Tile.ID,
		Name: "PORT", Value: "9090", CreatedAt: now, UpdatedAt: now}), "re-upsert")
	got, err = store.ListVariables(ctx, repo.OwnerTile, seed.Tile.ID)
	require.NoError(t, err, "list after overwrite")
	require.Len(t, got, len(want), "overwrite added a row")
	for _, v := range got {
		if v.Name == "PORT" {
			assert.Equal(t, "9090", v.Value, "PORT")
		}
	}

	// Owners are separate namespaces, a stack var with the same name coexists.
	require.NoError(t, store.UpsertVariable(ctx, &repo.Variable{OwnerKind: repo.OwnerStack, OwnerID: seed.Stack.ID,
		Name: "PORT", Value: "1", CreatedAt: now, UpdatedAt: now}), "stack upsert")
	sv, err := store.ListVariables(ctx, repo.OwnerStack, seed.Stack.ID)
	require.NoError(t, err, "stack vars")
	require.Len(t, sv, 1, "stack vars = %v, want 1", sv)

	require.NoError(t, store.DeleteVariable(ctx, repo.OwnerTile, seed.Tile.ID, "PORT"), "delete")
	got, err = store.ListVariables(ctx, repo.OwnerTile, seed.Tile.ID)
	require.NoError(t, err, "after delete")
	require.Len(t, got, len(want)-1, "after delete")
}

// R2 bridge: an ordinary tile write only adds rows (an API-written variable
// isn't in the blob and must survive), while config apply replaces, the file
// is the whole truth, so a var dropped from it stops resolving.
func TestEnvBlobProjection(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()

	tile := seed.Tile
	tile.Env = "FROM_BLOB=1"
	require.NoError(t, store.UpdateTile(ctx, tile.ID, tile.TileConfig), "update")
	// A structured write that never appears in the blob.
	require.NoError(t, store.UpsertVariable(ctx, &repo.Variable{OwnerKind: repo.OwnerTile, OwnerID: tile.ID,
		Name: "FROM_API", Value: "2", CreatedAt: now, UpdatedAt: now}), "api var")
	require.NoError(t, store.UpsertVariable(ctx, &repo.Variable{OwnerKind: repo.OwnerTile, OwnerID: tile.ID,
		Name: "TOKEN", Value: "s3cr3t", Secret: true, CreatedAt: now, UpdatedAt: now}), "secret var")

	names := func() map[string]bool {
		t.Helper()
		vs, err := store.ListVariables(ctx, repo.OwnerTile, tile.ID)
		require.NoError(t, err, "list")
		out := map[string]bool{}
		for _, v := range vs {
			out[v.Name] = true
		}
		return out
	}

	// An unrelated tile save carries the old blob, it must not eat FROM_API.
	tile.Name = "renamed"
	require.NoError(t, store.UpdateTile(ctx, tile.ID, tile.TileConfig), "second update")
	got := names()
	require.True(t, got["FROM_API"] && got["FROM_BLOB"] && got["TOKEN"], "plain update lost rows: %v", got)

	// Config apply: the blob no longer declares FROM_BLOB, so it goes. The
	// secret stays, it never came from the blob.
	tile.Env = "OTHER=3"
	require.NoError(t, store.ReplaceTileVars(ctx, tile), "replace")
	got = names()
	assert.False(t, got["FROM_BLOB"] || got["FROM_API"], "replace kept undeclared vars: %v", got)
	assert.True(t, got["OTHER"] && got["TOKEN"], "replace dropped declared var or secret: %v", got)
}

// R1: deleting a resource takes its outputs and bindings with it, nothing else
// collects them, and a stale binding would keep granting access to a resource
// whose id gets reused.
func TestResourceDeleteCascades(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()

	res := &repo.ManagedResource{ID: "res1", EnvironmentID: seed.Env.ID, ProviderTileID: seed.Tile.ID,
		Name: "app db", Slug: "appdb", Kind: "postgres", Status: "active", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateResource(ctx, res), "create resource")
	require.NoError(t, store.UpsertOutput(ctx, &repo.ResourceOutput{ResourceID: res.ID, Name: "DATABASE_URL",
		Value: "postgres://u:p@host:5432/db", Secret: true, RequiresNetwork: true}), "upsert output")
	require.NoError(t, store.CreateBinding(ctx, &repo.ResourceBinding{ResourceID: res.ID,
		ConsumerTileID: seed.Tile.ID, CreatedAt: now}), "create binding")

	outs, err := store.ListOutputs(ctx, res.ID)
	require.NoError(t, err, "outputs")
	require.Len(t, outs, 1, "outputs = %v", outs)
	require.Equal(t, "postgres://u:p@host:5432/db", outs[0].Value, "outputs = %v", outs)
	b, err := store.BindingsForConsumer(ctx, seed.Tile.ID)
	require.NoError(t, err, "bindings")
	require.Len(t, b, 1, "bindings = %v, want 1", b)

	// Re-binding the same pair is a no-op, not a constraint error.
	require.NoError(t, store.CreateBinding(ctx, &repo.ResourceBinding{ResourceID: res.ID,
		ConsumerTileID: seed.Tile.ID, CreatedAt: now}), "rebind")

	require.NoError(t, store.DeleteResource(ctx, res.ID), "delete resource")
	outs, err = store.ListOutputs(ctx, res.ID)
	assert.NoError(t, err, "outputs after delete")
	assert.Len(t, outs, 0, "outputs survived delete: %v", outs)
	b, err = store.BindingsForConsumer(ctx, seed.Tile.ID)
	assert.NoError(t, err, "bindings after delete")
	assert.Len(t, b, 0, "bindings survived delete: %v", b)
}

// Slug is unique per environment, two resources can't both answer to
// ${{ tile.<slug>.… }} in the same env.
func TestResourceSlugUniquePerEnv(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()

	mk := func(id string) *repo.ManagedResource {
		return &repo.ManagedResource{ID: id, EnvironmentID: seed.Env.ID, ProviderTileID: seed.Tile.ID,
			Name: id, Slug: "appdb", Kind: "postgres", Status: "active", CreatedAt: now, UpdatedAt: now}
	}
	require.NoError(t, store.CreateResource(ctx, mk("r1")), "first")
	require.Error(t, store.CreateResource(ctx, mk("r2")), "duplicate slug in one env was accepted")
}
