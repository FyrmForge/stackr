package varref_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

func setVar(t *testing.T, s *sqlite.Store, kind, owner, name, val string, secret bool) {
	t.Helper()
	now := time.Now().UTC()
	err := s.UpsertVariable(context.Background(), &repo.Variable{OwnerKind: kind, OwnerID: owner,
		Name: name, Value: val, Secret: secret, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err, "set %s", name)
}

// resolve is the common assertion: System mode, no error, returns the vars.
func resolve(t *testing.T, s *sqlite.Store, tileID string) map[string]string {
	t.Helper()
	res, err := varref.New(s).Resolve(context.Background(), tileID, varref.System)
	require.NoError(t, err, "resolve")
	return res.Vars
}

// mustFail asserts resolution fails and the message mentions want, an error
// nobody can act on is nearly as bad as no error.
func mustFail(t *testing.T, s *sqlite.Store, tileID string, mode varref.Mode, want string) {
	t.Helper()
	_, err := varref.New(s).Resolve(context.Background(), tileID, mode)
	require.Error(t, err, "expected an error mentioning %q, got none", want)
	require.ErrorContains(t, err, want)
}

func TestResolveValues(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)

	setVar(t, s, repo.OwnerStack, seed.Stack.ID, "REGION", "eu-west", false)
	setVar(t, s, repo.OwnerOrg, seed.Org.ID, "SENTRY_DSN", "https://key@sentry/1", true)
	// A stack variable that itself references another, recursion.
	setVar(t, s, repo.OwnerStack, seed.Stack.ID, "BUCKET", "assets-${{ stack.vars.REGION }}", false)

	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "PLAIN", "8080", false)
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "EMBEDDED", "https://x/${{ stack.vars.REGION }}/y", false)
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "NESTED", "${{ stack.vars.BUCKET }}", false)
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "DSN", "${{ org.secrets.SENTRY_DSN }}", false)
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "MULTI", "line1\n${{ stack.vars.REGION }}\nline3", false)

	got := resolve(t, s, seed.Tile.ID)
	for name, want := range map[string]string{
		"PLAIN":    "8080",
		"EMBEDDED": "https://x/eu-west/y",
		"NESTED":   "assets-eu-west",
		"DSN":      "https://key@sentry/1",
		"MULTI":    "line1\neu-west\nline3",
	} {
		assert.Equal(t, want, got[name], name)
	}
}

// ExpandStrings covers volume lines and command overrides: same resolver,
// same never-run-unresolved contract, secrets included (System mode).
func TestExpandStrings(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	setVar(t, s, repo.OwnerOrg, seed.Org.ID, "MEDIA_ROOT", "/mnt/16tb/Media", false)

	got, err := varref.New(s).ExpandStrings(context.Background(), seed.Tile.ID, varref.System,
		[]string{"${{ org.vars.MEDIA_ROOT }}/tv:/tv", "plain:/data", "-config.file=/etc/x.yml"})
	require.NoError(t, err)
	assert.Equal(t, []string{"/mnt/16tb/Media/tv:/tv", "plain:/data", "-config.file=/etc/x.yml"}, got)

	_, err = varref.New(s).ExpandStrings(context.Background(), seed.Tile.ID, varref.System,
		[]string{"${{ org.vars.NOPE }}:/x"})
	require.Error(t, err, "an unresolved reference in a bind must fail, not pass through literal")
}

func TestResolveErrors(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	tile := seed.Tile.ID

	t.Run("missing", func(t *testing.T) {
		setVar(t, s, repo.OwnerTile, tile, "A", "${{ stack.vars.NOPE }}", false)
		mustFail(t, s, tile, varref.System, "no stack var named")
		require.NoError(t, s.DeleteVariable(context.Background(), repo.OwnerTile, tile, "A"))
	})

	t.Run("malformed", func(t *testing.T) {
		setVar(t, s, repo.OwnerTile, tile, "B", "${{ nonsense }}", false)
		mustFail(t, s, tile, varref.System, "expected scope.name")
		require.NoError(t, s.DeleteVariable(context.Background(), repo.OwnerTile, tile, "B"))
	})

	t.Run("unknown source", func(t *testing.T) {
		setVar(t, s, repo.OwnerTile, tile, "C", "${{ tile.ghost.URL }}", false)
		mustFail(t, s, tile, varref.System, "no tile or resource named")
		require.NoError(t, s.DeleteVariable(context.Background(), repo.OwnerTile, tile, "C"))
	})

	t.Run("cycle", func(t *testing.T) {
		setVar(t, s, repo.OwnerStack, seed.Stack.ID, "X", "${{ stack.vars.Y }}", false)
		setVar(t, s, repo.OwnerStack, seed.Stack.ID, "Y", "${{ stack.vars.X }}", false)
		setVar(t, s, repo.OwnerTile, tile, "D", "${{ stack.vars.X }}", false)
		mustFail(t, s, tile, varref.System, "cycle")
		require.NoError(t, s.DeleteVariable(context.Background(), repo.OwnerTile, tile, "D"))
	})

	t.Run("legacy secret syntax", func(t *testing.T) {
		setVar(t, s, repo.OwnerTile, tile, "E", "${secret.db_url}", false)
		mustFail(t, s, tile, varref.System, "removed ${secret.*} syntax")
		require.NoError(t, s.DeleteVariable(context.Background(), repo.OwnerTile, tile, "E"))
	})
}

// Scoped mode must fail on a secret rather than quietly dropping it, a caller
// that gets a half-populated env can't tell it from a complete one.
func TestScopedModeRefusesSecrets(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)

	setVar(t, s, repo.OwnerStack, seed.Stack.ID, "TOKEN", "s3cr3t", true)
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "PUBLIC", "fine", false)
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "USES_SECRET", "${{ stack.secrets.TOKEN }}", false)

	mustFail(t, s, seed.Tile.ID, varref.Scoped, "secrets:read")
	require.Equal(t, "s3cr3t", resolve(t, s, seed.Tile.ID)["USES_SECRET"],
		"System mode should resolve the secret")

	// The commonest case is a tile's own secret with no indirection at all,
	// it must refuse there too, not just when a reference leads to one.
	ctx := context.Background()
	for _, name := range []string{"TOKEN"} {
		require.NoError(t, s.DeleteVariable(ctx, repo.OwnerStack, seed.Stack.ID, name))
	}
	require.NoError(t, s.DeleteVariable(ctx, repo.OwnerTile, seed.Tile.ID, "USES_SECRET"))
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "OWN_SECRET", "literal", true)
	mustFail(t, s, seed.Tile.ID, varref.Scoped, "secrets:read")
}

// A resource reference needs a binding, and a network-bound output pulls the
// provider's shared network into the deploy plan.
func TestResourceBindingAndNetwork(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	now := time.Now().UTC()

	// The instance lives in a different env, the case that needs the shared
	// network. A same-env instance already answers to its alias locally.
	shared := &repo.Environment{ID: "envshared", StackID: seed.Stack.ID, Name: "infra",
		Slug: "infra", Type: "static", CreatedAt: now}
	require.NoError(t, s.CreateEnvironment(ctx, shared), "env")
	provider := &repo.Tile{ID: "pg1", StackID: seed.Stack.ID, EnvironmentID: shared.ID,
		Name: "pg", Slug: "pg", Kind: "service", Engine: "postgres", Status: "idle",
		// Claimed out of the db pool on provision (infra/netpool); an
		// instance that has never deployed holds none and pulls in no network.
		SharedNetName: "stkr-dbnet-01",
		CreatedAt:     now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, provider), "provider")
	res := &repo.ManagedResource{ID: "res1", EnvironmentID: seed.Env.ID, ProviderTileID: provider.ID,
		Name: "orders db", Slug: "orders-db", Kind: "postgres", Status: "active", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateResource(ctx, res), "resource")
	require.NoError(t, s.UpsertOutput(ctx, &repo.ResourceOutput{ResourceID: res.ID, Name: "DATABASE_URL",
		Value: "postgres://u:p@pg:5432/orders", Secret: true, RequiresNetwork: true}), "output")
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "DATABASE_URL", "${{ tile.orders-db.DATABASE_URL }}", false)

	// No binding yet: referencing is not access.
	mustFail(t, s, seed.Tile.ID, varref.System, "not attached")

	require.NoError(t, s.CreateBinding(ctx, &repo.ResourceBinding{ResourceID: res.ID,
		ConsumerTileID: seed.Tile.ID, CreatedAt: now}), "bind")
	got, err := varref.New(s).Resolve(ctx, seed.Tile.ID, varref.System)
	require.NoError(t, err, "resolve after bind")
	assert.Equal(t, "postgres://u:p@pg:5432/orders", got.Vars["DATABASE_URL"])
	require.Len(t, got.Deps, 1, "want one resource dep on orders-db")
	assert.Equal(t, "resource", got.Deps[0].Kind, "want one resource dep on orders-db")
	assert.Equal(t, "orders-db", got.Deps[0].Slug, "want one resource dep on orders-db")
	require.Len(t, got.Networks, 1, "want %s", provider.SharedNet())
	assert.Equal(t, provider.SharedNet(), got.Networks[0])

	// Run modes that can't attach networks must refuse rather than start a
	// container whose hostname provably won't resolve.
	_, err = varref.EnvLines(ctx, s, seed.Tile)
	assert.Error(t, err, "EnvLines returned values needing a network it can't join")
}

// A stack-scoped singleton living in another environment is only reachable
// over its shared network, so referencing it must pull that network in, R4
// attaches containers from exactly this list.
func TestCrossEnvSingletonRequiresNetwork(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	now := time.Now().UTC()

	other := &repo.Environment{ID: "env2", StackID: seed.Stack.ID, Name: "shared", Slug: "shared",
		Type: "static", CreatedAt: now}
	require.NoError(t, s.CreateEnvironment(ctx, other), "env")
	redis := &repo.Tile{ID: "redis123", StackID: seed.Stack.ID, EnvironmentID: other.ID,
		Name: "redis", Slug: "redis", Kind: "service", ScopeKind: "stack", ScopeID: seed.Stack.ID,
		Status: "running", SharedNetName: "stkr-dbnet-02", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, redis), "tile")
	setVar(t, s, repo.OwnerTile, redis.ID, "REDIS_URL", "redis://redis:6379/0", false)
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "CACHE", "${{ stack.redis.REDIS_URL }}", false)

	got, err := varref.New(s).Resolve(ctx, seed.Tile.ID, varref.System)
	require.NoError(t, err, "resolve")
	assert.Equal(t, "redis://redis:6379/0", got.Vars["CACHE"])
	require.Len(t, got.Networks, 1, "want %s", redis.SharedNet())
	assert.Equal(t, redis.SharedNet(), got.Networks[0])
}

// A tile slug and a resource slug in one environment is ambiguous: the
// reference must fail naming both, not silently pick one.
func TestSlugCollisionIsAmbiguous(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	now := time.Now().UTC()

	twin := &repo.Tile{ID: "twin", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "cache", Slug: "cache", Kind: "service", Status: "idle", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, twin), "tile")
	require.NoError(t, s.CreateResource(ctx, &repo.ManagedResource{ID: "res1", EnvironmentID: seed.Env.ID,
		ProviderTileID: twin.ID, Name: "cache", Slug: "cache", Kind: "redis", Status: "active",
		CreatedAt: now, UpdatedAt: now}), "resource")
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "URL", "${{ tile.cache.URL }}", false)

	mustFail(t, s, seed.Tile.ID, varref.System, "both a tile and a managed resource")
}

// A tile's own variables are referenceable by siblings in the same env.
func TestTileVariableReference(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	now := time.Now().UTC()

	api := &repo.Tile{ID: "api1", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "api", Slug: "api", Kind: "service", Status: "idle", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, api), "tile")
	setVar(t, s, repo.OwnerTile, api.ID, "PORT", "3000", false)
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "API_PORT", "${{ tile.api.PORT }}", false)

	got, err := varref.New(s).Resolve(ctx, seed.Tile.ID, varref.System)
	require.NoError(t, err, "resolve")
	assert.Equal(t, "3000", got.Vars["API_PORT"])
	require.Len(t, got.Deps, 1, "want one edge to the api tile")
	assert.Equal(t, api.ID, got.Deps[0].ID, "want one edge to the api tile")
	assert.Empty(t, got.Networks, "same-env reference needs no shared network")
}

// A literal URL naming a sibling by slug is a dependency in spirit: same
// canvas edge as a ${{ }} reference. Bare slug mentions never match.
func TestLiteralURLDeps(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)

	now := time.Now().UTC()
	sib := &repo.Tile{ID: "sib1", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "orders-api", Slug: "orders-api", Kind: "service", SourceType: "image",
		ContainerPort: 8181, WebhookToken: "tok-sib", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(context.Background(), sib), "sibling")
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "ORDERS_API_URL", "http://orders-api:8181", false)
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "MENTION", "talk to orders-api later", false)

	res, err := varref.New(s).Resolve(context.Background(), seed.Tile.ID, varref.System)
	require.NoError(t, err, "resolve")
	var hit bool
	for _, d := range res.Deps {
		if d.Kind == "tile" && d.Slug == "orders-api" {
			hit = true
		}
	}
	assert.True(t, hit, "literal URL did not produce a dep: %+v", res.Deps)
	assert.Len(t, res.Deps, 1, "bare slug mention must not match")
}

// The bucket must match the row's secret flag. Crucially this is NOT an
// UnsetError: the value exists, the reference is wrong, and parking the tile as
// waiting would wait forever for something nobody is going to set.
func TestBucketMustMatchTheSecretFlag(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)

	setVar(t, s, repo.OwnerStack, seed.Stack.ID, "TOKEN", "s3cr3t", true)
	setVar(t, s, repo.OwnerStack, seed.Stack.ID, "REGION", "eu-west", false)

	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "A", "${{ stack.vars.TOKEN }}", false)
	_, err := varref.New(s).Resolve(ctx, seed.Tile.ID, varref.System)
	require.ErrorContains(t, err, "stack.secrets.TOKEN", "want the right namespace named")
	_, unset := varref.Unset(err)
	require.False(t, unset, "a namespace mismatch must not park the tile as waiting")
	require.NoError(t, s.DeleteVariable(ctx, repo.OwnerTile, seed.Tile.ID, "A"))

	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "B", "${{ stack.secrets.REGION }}", false)
	_, err = varref.New(s).Resolve(ctx, seed.Tile.ID, varref.System)
	require.ErrorContains(t, err, "stack.vars.REGION")
	require.NoError(t, s.DeleteVariable(ctx, repo.OwnerTile, seed.Tile.ID, "B"))
}

// The two-part form is gone, and the error has to name the replacement: the
// file it came from is checked into git and somebody has to fix it.
func TestOldTwoPartFormIsAParseError(t *testing.T) {
	for _, body := range []string{"stack.NAME", "org.NAME"} {
		_, err := varref.Parse(body)
		require.ErrorContains(t, err, ".vars.NAME", "%s parsed as %q", body, err)
		require.ErrorContains(t, err, ".secrets.NAME")
	}
	// A backup destination is reference-only, never a container value.
	_, err := varref.Parse("org.backups.prod-bucket")
	require.NoError(t, err, "the backups bucket must parse; the resolver is what refuses it")
}
