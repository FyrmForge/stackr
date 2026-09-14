package managedtiles_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// svc builds a Service with no docker runtime: SyncResource and its helpers
// only touch the store, and this keeps the mirror testable without a daemon.
func svc(s *sqlite.Store) *managedtiles.Service { return managedtiles.NewService(nil, s) }

func instance(t *testing.T, s *sqlite.Store, seed testdb.Seed, engine string) *repo.Tile {
	t.Helper()
	now := time.Now().UTC()
	inst := &repo.Tile{ID: "inst" + engine + "0000", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: engine, Slug: engine, Kind: "service", Engine: engine, DBUser: "root",
		DBPassword: "rootpw", Status: "running", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(context.Background(), inst), "instance")
	return inst
}

func outputs(t *testing.T, s *sqlite.Store, envID, slug string) map[string]repo.ResourceOutput {
	t.Helper()
	ctx := context.Background()
	res, err := s.ListResourcesByEnv(ctx, envID)
	require.NoError(t, err, "list resources")
	for _, r := range res {
		if r.Slug != slug {
			continue
		}
		outs, err := s.ListOutputs(ctx, r.ID)
		require.NoError(t, err, "list outputs")
		m := map[string]repo.ResourceOutput{}
		for _, o := range outs {
			m[o.Name] = o
		}
		return m
	}
	require.Failf(t, "resource not found", "no resource %q in env", slug)
	return nil
}

// R3: a provisioned postgres slice publishes all six PG outputs with real
// values. PGHOST/PGPORT are derived from the instance tile, not stored.
func TestPostgresOutputs(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	inst := instance(t, s, seed, "postgres")

	p := &repo.Provision{ID: "p1", InstanceTileID: inst.ID, ConsumerTileID: seed.Tile.ID,
		EnvID: seed.Env.ID, DBName: "orders", DBUser: "orders", DBPassword: "pw",
		Status: "active", CreatedAt: time.Now().UTC()}
	require.NoError(t, s.CreateProvision(ctx, p), "provision")
	require.NoError(t, svc(s).SyncResource(ctx, inst, p), "sync")

	outs := outputs(t, s, seed.Env.ID, managedtiles.ResourceSlug(inst, p))
	for _, name := range []string{"DATABASE_URL", "PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD"} {
		o, ok := outs[name]
		if !assert.True(t, ok, "%s not published", name) {
			continue
		}
		assert.NotEmpty(t, o.Value, "%s is empty", name)
	}
	assert.Equal(t, "5432", outs["PGPORT"].Value, "derived PGPORT wrong")
	assert.Equal(t, "orders", outs["PGDATABASE"].Value, "derived PGDATABASE wrong")
	// The instance's own superuser credentials are never published.
	for name, o := range outs {
		assert.NotEqual(t, inst.DBPassword, o.Value, "%s leaks the instance admin password", name)
	}
	assert.True(t, outs["PGPASSWORD"].Secret, "credentials must be marked secret")
	assert.True(t, outs["DATABASE_URL"].Secret, "credentials must be marked secret")
	assert.True(t, outs["DATABASE_URL"].RequiresNetwork,
		"DATABASE_URL host only resolves on the shared network")
}

func TestS3Outputs(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	inst := instance(t, s, seed, "s3")
	now := time.Now().UTC()
	require.NoError(t, s.CreateDomain(ctx, &repo.Domain{ID: "d1", TileID: inst.ID, Host: "cdn.example.com",
		Path: "/", ContainerPort: 9000, HTTPS: true, CreatedAt: now}), "domain")

	p := &repo.Provision{ID: "p1", InstanceTileID: inst.ID, ConsumerTileID: seed.Tile.ID,
		EnvID: seed.Env.ID, DBName: "assets", DBUser: "ak", DBPassword: "sk",
		Status: "active", Public: true, CreatedAt: now}
	require.NoError(t, s.CreateProvision(ctx, p), "provision")
	require.NoError(t, svc(s).SyncResource(ctx, inst, p), "sync")

	outs := outputs(t, s, seed.Env.ID, managedtiles.ResourceSlug(inst, p))
	for _, name := range []string{"S3_ENDPOINT", "S3_BUCKET", "S3_REGION", "S3_ACCESS_KEY", "S3_SECRET_KEY", "S3_PUBLIC_URL"} {
		assert.NotEmpty(t, outs[name].Value, "%s missing or empty", name)
	}
	assert.Equal(t, "https://cdn.example.com/assets", outs["S3_PUBLIC_URL"].Value)
	assert.True(t, outs["S3_SECRET_KEY"].Secret, "S3_SECRET_KEY must be marked secret")
}

// The auto-injected variable set is derived from the slice's own outputs, so
// a consumer can never be wired to a reference the slice doesn't publish.
func TestAutoInjectVars(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	inst := instance(t, s, seed, "s3")
	now := time.Now().UTC()

	p := &repo.Provision{ID: "p1", InstanceTileID: inst.ID, ConsumerTileID: seed.Tile.ID,
		EnvID: seed.Env.ID, DBName: "site", DBUser: "ak", DBPassword: "sk",
		Status: "active", CreatedAt: now}
	priv := svc(s).AutoInjectVars(ctx, inst, p)
	assert.NotContains(t, priv, "S3_PUBLIC_URL", "private bucket must not inject S3_PUBLIC_URL")
	assert.Equal(t, managedtiles.Ref(inst, p, "S3_ENDPOINT"), priv["S3_ENDPOINT"])

	// Public only publishes the unsigned base when the instance is reachable
	// from outside, so the domain is what makes the extra var appear.
	require.NoError(t, s.CreateDomain(ctx, &repo.Domain{ID: "d1", TileID: inst.ID, Host: "cdn.example.com",
		Path: "/", ContainerPort: 9000, HTTPS: true, CreatedAt: now}), "domain")
	p.Public = true
	assert.Equal(t, managedtiles.Ref(inst, p, "S3_PUBLIC_URL"),
		svc(s).AutoInjectVars(ctx, inst, p)["S3_PUBLIC_URL"], "public bucket S3_PUBLIC_URL")
}

// A db tile used directly (no provisioning) publishes its connection details
// on itself, so a sibling can read ${{ tile.<db>.DATABASE_URL }}.
func TestPublishConnection(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	inst := instance(t, s, seed, "postgres")
	inst.DBName, inst.DBUser, inst.DBPassword = "app", "app", "pw"

	managedtiles.PublishConnection(ctx, s, inst)

	vars, err := s.ListVariables(ctx, repo.OwnerTile, inst.ID)
	require.NoError(t, err, "list")
	got := map[string]repo.Variable{}
	for _, v := range vars {
		got[v.Name] = v
	}
	assert.NotEmpty(t, got["DATABASE_URL"].Value, "DATABASE_URL = %+v, want a non-empty secret", got["DATABASE_URL"])
	assert.True(t, got["DATABASE_URL"].Secret, "DATABASE_URL = %+v, want a non-empty secret", got["DATABASE_URL"])
	assert.Equal(t, "5432", got["PGPORT"].Value, "derived PG vars wrong: %+v", got)
	assert.Equal(t, "app", got["PGUSER"].Value, "derived PG vars wrong: %+v", got)
	assert.True(t, got["PGPASSWORD"].Secret, "PGPASSWORD must be marked secret")

	// The published host is the rename-stable tile alias, the same one a
	// provisioned slice publishes. Renaming the tile used to leave every
	// consumer of ${{ tile.<slug>.DATABASE_URL }} pointing at a hostname the
	// container no longer answers to, while slice consumers kept working.
	alias := envnet.TileAlias(inst.ID)
	assert.Equal(t, alias, got["PGHOST"].Value, "PGHOST, want the tile alias")
	before := got["DATABASE_URL"].Value
	inst.Slug, inst.Name = "renamed", "renamed"
	require.NoError(t, s.UpdateTile(ctx, inst), "rename")
	managedtiles.PublishConnection(ctx, s, inst)
	after, err := s.ListVariables(ctx, repo.OwnerTile, inst.ID)
	require.NoError(t, err, "list after rename")
	for _, v := range after {
		if v.Name == "DATABASE_URL" {
			assert.Equal(t, before, v.Value, "rename changed the connection host")
		}
	}
}

// ProvisionSlice adopts an existing slice with the same name in the same env,
// re-stamped with the config key and revived if orphaned, instead of
// uniquifying a silent second copy. Adoption is store-only, so it runs without
// a docker runtime; the fresh-provision path execs into the container and is
// covered by the engine integration tests.
func TestProvisionSliceAdopt(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	inst := instance(t, s, seed, "postgres")
	now := time.Now().UTC()

	orphan := &repo.Provision{ID: "p1", InstanceTileID: inst.ID, EnvID: seed.Env.ID,
		DBName: "orders", DBUser: "orders", DBPassword: "pw",
		Status: "orphaned", CreatedAt: now}
	require.NoError(t, s.CreateProvision(ctx, orphan), "provision")
	require.NoError(t, svc(s).SyncResource(ctx, inst, orphan), "sync")

	p, err := svc(s).ProvisionSlice(ctx, inst, seed.Env.ID, "site-db", "orders", false)
	require.NoError(t, err, "adopt")
	assert.Equal(t, orphan.ID, p.ID, "adopted a new row, want the existing one")
	assert.Equal(t, "site-db", p.ResourceSlug, "adopted row slug")
	assert.Equal(t, "active", p.Status, "adopted row status")
	// The resource is renamed onto the config key, not duplicated.
	res, err := s.ListResourcesByEnv(ctx, seed.Env.ID)
	require.NoError(t, err)
	require.Len(t, res, 1, "want exactly 1 resource")
	assert.Equal(t, "site-db", res[0].Slug, "resource slug")
	assert.Equal(t, "orders", res[0].Name, "resource name")

	// The same db name in another environment is a conflict, never an adoption.
	env2 := &repo.Environment{ID: "env2", StackID: seed.Stack.ID, Name: "staging",
		Slug: "staging", Type: "static", CreatedAt: now}
	require.NoError(t, s.CreateEnvironment(ctx, env2), "env2")
	_, err = svc(s).ProvisionSlice(ctx, inst, env2.ID, "x", "orders", false)
	require.ErrorContains(t, err, "another environment", "cross-env adopt, want the another-environment error")
}

// Attaching a second consumer to the same slice adds a binding, not a second
// resource, two resources with one slug would break UNIQUE(environment_id,
// slug) and make the reference ambiguous.
func TestAttachSharesOneResource(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	inst := instance(t, s, seed, "postgres")
	now := time.Now().UTC()

	second := &repo.Tile{ID: "cron1", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "worker", Slug: "worker", Kind: "cron", Status: "idle", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, second), "second consumer")

	base := repo.Provision{InstanceTileID: inst.ID, EnvID: seed.Env.ID, DBName: "orders",
		DBUser: "orders", DBPassword: "pw", Status: "active", CreatedAt: now}
	first, attached := base, base
	first.ID, first.ConsumerTileID = "p1", seed.Tile.ID
	attached.ID, attached.ConsumerTileID = "p2", second.ID

	for _, p := range []*repo.Provision{&first, &attached} {
		require.NoError(t, s.CreateProvision(ctx, p), "provision %s", p.ID)
		require.NoError(t, svc(s).SyncResource(ctx, inst, p), "sync %s", p.ID)
	}

	res, err := s.ListResourcesByEnv(ctx, seed.Env.ID)
	require.NoError(t, err)
	require.Len(t, res, 1, "want exactly 1 resource")
	binds, err := s.ListBindingsByResource(ctx, res[0].ID)
	require.NoError(t, err)
	require.Len(t, binds, 2, "want 2 bindings")

	// Detaching one consumer revokes only its access, and takes its now-dead
	// reference with it, an unbound reference is a hard resolve error, so
	// leaving it behind would fail every later deploy of that tile.
	second.Env = "DATABASE_URL=" + managedtiles.Ref(inst, &attached, "DATABASE_URL") + "\nKEEP=1"
	require.NoError(t, s.UpdateTile(ctx, second), "set consumer env")
	require.NoError(t, svc(s).Detach(ctx, &attached), "detach")
	left, err := s.ListVariables(ctx, repo.OwnerTile, second.ID)
	require.NoError(t, err, "list vars")
	for _, v := range left {
		assert.NotEqual(t, "DATABASE_URL", v.Name, "detach left the dead reference behind")
	}
	require.Len(t, left, 1, "detach removed unrelated variables: %+v", left)
	assert.Equal(t, "KEEP", left[0].Name, "detach removed unrelated variables: %+v", left)
	b, err := s.BindingsForConsumer(ctx, second.ID)
	assert.NoError(t, err)
	assert.Empty(t, b, "detached consumer keeps bindings")
	b, err = s.BindingsForConsumer(ctx, seed.Tile.ID)
	assert.NoError(t, err)
	assert.Len(t, b, 1, "other consumer lost its binding")
}
