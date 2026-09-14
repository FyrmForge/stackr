package managedtiles

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// world builds org "org" → stack "stack" → env "prod" with a postgres instance
// "sharedpg" and one slice on it, plus a second org nobody should see across.
func world(t *testing.T) (*sqlite.Store, testdb.Seed) {
	t.Helper()
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	now := time.Now().UTC()

	inst := &repo.Tile{ID: "instpg", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "sharedpg", Slug: "sharedpg", Kind: "managed", Engine: "postgres",
		ScopeKind: "env", DBUser: "root", DBPassword: "pw", Status: "running",
		CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, inst))
	require.NoError(t, s.CreateProvision(ctx, &repo.Provision{
		ID: "prov1", InstanceTileID: inst.ID, EnvID: seed.Env.ID,
		DBName: "app_db", ResourceSlug: "app-db", Status: "active", CreatedAt: now,
	}))

	// A second org with the same-looking names, to prove isolation.
	other := &repo.Org{ID: "org2", Name: "Other", Slug: "other", CreatedAt: now}
	require.NoError(t, s.CreateOrg(ctx, other))
	ost := &repo.Stack{ID: "stack2", OrgID: other.ID, Name: "Shop", Slug: "shop", CreatedAt: now}
	require.NoError(t, s.CreateStack(ctx, ost))
	oenv := &repo.Environment{ID: "env2", StackID: ost.ID, Name: "prod", Slug: "prod", Type: "static", CreatedAt: now}
	require.NoError(t, s.CreateEnvironment(ctx, oenv))
	require.NoError(t, s.CreateTile(ctx, &repo.Tile{ID: "instpg2", StackID: ost.ID, EnvironmentID: oenv.ID,
		Name: "secretdb", Slug: "secretdb", Kind: "managed", Engine: "postgres", ScopeKind: "env",
		Status: "running", CreatedAt: now, UpdatedAt: now}))
	return s, seed
}

func mine() ResolveScope { return ResolveScope{AllowedOrgs: map[string]bool{"org1": true}} }
func here(env string) ResolveScope {
	return ResolveScope{AllowedOrgs: map[string]bool{"org1": true}, EnvID: env}
}

func TestResolveAbsoluteInstanceAndSlice(t *testing.T) {
	ctx := context.Background()
	s, seed := world(t)

	got, err := ResolveTarget(ctx, s, "org:stack:prod:sharedpg", mine())
	require.NoError(t, err)
	assert.Equal(t, TargetInstance, got.Kind)
	assert.Equal(t, "instpg", got.Instance.ID)
	assert.Equal(t, "org:stack:prod:sharedpg", got.Path)

	got, err = ResolveTarget(ctx, s, "org:stack:prod:app-db", mine())
	require.NoError(t, err)
	assert.Equal(t, TargetSlice, got.Kind)
	assert.Equal(t, "prov1", got.Provision.ID)
	assert.Equal(t, "instpg", got.Instance.ID, "a slice carries its provider")
	_ = seed
}

// The whole point of the relative form: from a linked directory a bare name
// resolves without typing the org and stack.
func TestResolveRelativeBareName(t *testing.T) {
	ctx := context.Background()
	s, seed := world(t)

	got, err := ResolveTarget(ctx, s, "app-db", here(seed.Env.ID))
	require.NoError(t, err)
	assert.Equal(t, TargetSlice, got.Kind)
	assert.Equal(t, "org:stack:prod:app-db", got.Path, "relative in, fully-qualified out")

	got, err = ResolveTarget(ctx, s, "sharedpg", here(seed.Env.ID))
	require.NoError(t, err)
	assert.Equal(t, TargetInstance, got.Kind)

	// Unlinked, a bare name has nothing to resolve against.
	_, err = ResolveTarget(ctx, s, "app-db", mine())
	assert.Error(t, err)
}

// Resolution turns a name into an id, so an unbounded resolver would let any
// key confirm another org's stacks and databases exist by guessing. A foreign
// path must fail exactly like a nonexistent one.
func TestResolveRefusesOtherOrgs(t *testing.T) {
	ctx := context.Background()
	s, _ := world(t)

	_, foreign := ResolveTarget(ctx, s, "other:shop:prod:secretdb", mine())
	require.Error(t, foreign)
	_, missing := ResolveTarget(ctx, s, "nosuch:shop:prod:secretdb", mine())
	require.Error(t, missing)
	// Same wording once the echoed path is discounted: a caller must not be
	// able to tell "that org is not yours" from "that org does not exist".
	assert.Equal(t,
		strings.Replace(missing.Error(), "nosuch", "ORG", 1),
		strings.Replace(foreign.Error(), "other", "ORG", 1),
		"a foreign org and a missing org must be indistinguishable")

	// The same path resolves for a caller who can see that org, so the refusal
	// is the access check and not a broken lookup.
	got, err := ResolveTarget(ctx, s, "other:shop:prod:secretdb",
		ResolveScope{AllowedOrgs: map[string]bool{"org2": true}})
	require.NoError(t, err)
	assert.Equal(t, "instpg2", got.Instance.ID)

	// SystemScope is unrestricted, for internal callers only.
	got, err = ResolveTarget(ctx, s, "other:shop:prod:secretdb", SystemScope())
	require.NoError(t, err)
	assert.Equal(t, "instpg2", got.Instance.ID)
}

// Segment count means scope in an absolute path and "how much was omitted" in
// a relative one, so the two readings can collide. Never guess.
func TestResolveAmbiguityErrors(t *testing.T) {
	ctx := context.Background()
	s, seed := world(t)
	now := time.Now().UTC()

	// An org named "prod" holding an org-scoped instance "app-db" makes
	// "prod:app-db" readable as absolute org:slug, while from the linked env
	// it also reads as relative env:slug.
	require.NoError(t, s.CreateOrg(ctx, &repo.Org{ID: "org3", Name: "Prod", Slug: "prod", CreatedAt: now}))
	require.NoError(t, s.CreateStack(ctx, &repo.Stack{ID: "stack3", OrgID: "org3", Name: "S", Slug: "s", CreatedAt: now}))
	require.NoError(t, s.CreateEnvironment(ctx, &repo.Environment{ID: "env3", StackID: "stack3", Name: "e", Slug: "e", Type: "static", CreatedAt: now}))
	require.NoError(t, s.CreateTile(ctx, &repo.Tile{ID: "inst3", StackID: "stack3", EnvironmentID: "env3",
		Name: "app-db", Slug: "app-db", Kind: "managed", Engine: "postgres", ScopeKind: "org",
		Status: "running", CreatedAt: now, UpdatedAt: now}))

	scope := ResolveScope{AllowedOrgs: map[string]bool{"org1": true, "org3": true}, EnvID: seed.Env.ID}
	_, err := ResolveTarget(ctx, s, "prod:app-db", scope)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")

	// Seeing only one of the two orgs removes the ambiguity, because the other
	// reading no longer resolves at all.
	_, err = ResolveTarget(ctx, s, "prod:app-db", here(seed.Env.ID))
	assert.NoError(t, err)
}

// A slice and an instance sharing a name in one env are different things to
// drop, so the resolver refuses rather than preferring one.
func TestResolveSliceInstanceNameClash(t *testing.T) {
	ctx := context.Background()
	s, seed := world(t)
	now := time.Now().UTC()

	require.NoError(t, s.CreateTile(ctx, &repo.Tile{ID: "clash", StackID: seed.Stack.ID,
		EnvironmentID: seed.Env.ID, Name: "app-db", Slug: "app-db", Kind: "managed",
		Engine: "postgres", ScopeKind: "env", Status: "running", CreatedAt: now, UpdatedAt: now}))

	_, err := ResolveTarget(ctx, s, "org:stack:prod:app-db", mine())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both a slice and an instance")
}
