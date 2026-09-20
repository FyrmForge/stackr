package managedtiles

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// An engine that provisions but cannot copy has to say so before anything is
// created, the alternative is a fresh empty slice that looks like a fork.
func TestForkSliceUnsupportedEngine(t *testing.T) {
	assert.False(t, CanFork("nope"))
	assert.True(t, CanFork("postgres"))

	s := testdb.New(t)
	svc := NewService(nil, s)
	inst := &repo.Tile{ID: "instnope", Engine: "nope", Slug: "cache"}
	_, err := svc.ForkSlice(context.Background(), inst, &repo.Provision{DBName: "app"}, "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot fork")
}

// The fork's reference slug has to be free in the env, and it is derived from
// the source's *effective* slug, slices cut from the instance drawer carry an
// empty ResourceSlug and fall back to instance-dbname.
func TestUniqueResourceSlug(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	now := time.Now().UTC()

	inst := &repo.Tile{ID: "instpg", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "pg", Slug: "sharedpg", Kind: "service", Engine: "postgres",
		DBUser: "root", DBPassword: "pw", Status: "running", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, inst))

	// One drawer-created slice (no ResourceSlug) and one config-declared.
	for _, p := range []repo.Provision{
		{ID: "p1", InstanceTileID: inst.ID, EnvID: seed.Env.ID, DBName: "app_db", Status: "active", CreatedAt: now},
		{ID: "p2", InstanceTileID: inst.ID, EnvID: seed.Env.ID, DBName: "jobs", ResourceSlug: "sharedpg-app-db-fork", Status: "active", CreatedAt: now},
	} {
		require.NoError(t, s.CreateProvision(ctx, &p))
	}

	svc := NewService(nil, s)

	// The drawer slice's effective slug is sharedpg-app_db... whatever
	// ResourceSlug derives, assert against the helper, not a literal.
	drawer := &repo.Provision{InstanceTileID: inst.ID, DBName: "app_db"}
	assert.Equal(t, "sharedpg-app_db", ResourceSlug(inst, drawer))

	// A free slug is returned untouched; a taken one gets suffixed.
	got, err := svc.uniqueResourceSlug(ctx, seed.Env.ID, "sharedpg-jobs-fork")
	require.NoError(t, err)
	assert.Equal(t, "sharedpg-jobs-fork", got)

	got, err = svc.uniqueResourceSlug(ctx, seed.Env.ID, "sharedpg-app-db-fork")
	require.NoError(t, err)
	assert.Equal(t, "sharedpg-app-db-fork-2", got)
}

// Dropping a slice must strip its consumers' references, not only unhook the
// bindings. A reference to a resource that no longer exists is a hard resolve
// error, so a leftover one fails every later deploy of that tile, the exact
// breakage dropConsumerRefs was written to prevent for Detach, which the drop
// path did not do.
func TestDropResourceStripsConsumerRefs(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	now := time.Now().UTC()

	inst := &repo.Tile{ID: "instpg-00000000", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "pg", Slug: "sharedpg", Kind: "managed", Engine: "postgres", ScopeKind: "env",
		DBUser: "root", DBPassword: "pw", Status: "running", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, inst))

	consumer := &repo.Tile{ID: "consumer1", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "api", Slug: "api", Kind: "service", Status: "running", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, consumer))

	p := &repo.Provision{ID: "prov1", InstanceTileID: inst.ID, ConsumerTileID: consumer.ID,
		EnvID: seed.Env.ID, DBName: "app_db", ResourceSlug: "app-db", Status: "active", CreatedAt: now}
	require.NoError(t, s.CreateProvision(ctx, p))

	svc := NewService(nil, s)
	require.NoError(t, svc.SyncResource(ctx, inst, p))

	// The consumer references the slice the way wiring a var does.
	consumer.Env = "DATABASE_URL=${{ tile.app-db.DATABASE_URL }}\nOTHER=keep-me"
	require.NoError(t, s.UpdateTile(ctx, consumer.ID, consumer.TileConfig))

	svc.dropResource(ctx, inst, p)

	got, err := s.GetTile(ctx, consumer.ID)
	require.NoError(t, err)
	assert.NotContains(t, got.Env, "tile.app-db.", "reference to the dropped slice must be gone")
	assert.Contains(t, got.Env, "OTHER=keep-me", "unrelated env must survive")
}
