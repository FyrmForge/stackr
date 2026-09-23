package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

// Every {id} also takes the object's slug path. The gates resolve it, so a
// handler that never mentions paths still accepts one.
func TestSlugPathsResolveAtEveryLevel(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	a := apiFor(s)
	c := echoCtx(t)

	org, err := a.requireOrg(c, "org")
	require.NoError(t, err, "bare org slug")
	require.Equal(t, seed.Org.ID, org.ID)

	st, err := a.requireStackAccess(c, "org:stack")
	require.NoError(t, err, "org:stack")
	require.Equal(t, seed.Stack.ID, st.ID)

	env, err := a.requireEnvAccess(c, "org:stack:prod")
	require.NoError(t, err, "org:stack:env")
	require.Equal(t, seed.Env.ID, env.ID)

	tile, err := a.requireTile(c, "org:stack:prod:app", false)
	require.NoError(t, err, "org:stack:env:tile")
	require.Equal(t, seed.Tile.ID, tile.ID)

	// A slash is accepted too, for a path that arrives in a body or a flag
	// rather than a URL segment.
	st, err = a.requireStackAccess(c, "org/stack")
	require.NoError(t, err, "slash separator")
	require.Equal(t, seed.Stack.ID, st.ID)

	// Ids still work; nothing about this changed how an id resolves.
	st, err = a.requireStackAccess(c, seed.Stack.ID)
	require.NoError(t, err, "id")
	require.Equal(t, seed.Stack.ID, st.ID)
	_ = ctx
}

// echoCtx is an admin request context: the gates read the user and org set off
// it, and every case here is about addressing, not about who is asking.
func echoCtx(t *testing.T) echo.Context {
	t.Helper()
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	c.Set(ctxUser, &repo.User{ID: "u1", Role: "admin"})
	return c
}

// Slugs repeat across orgs, so a path below org level has to start at the org.
// Accepting a bare one would make a script silently address whichever tenant
// happened to match.
func TestBareSlugsBelowOrgAreRefused(t *testing.T) {
	s := testdb.New(t)
	testdb.SeedStack(t, s, false)
	a := apiFor(s)
	c := echoCtx(t)

	for _, ref := range []string{"stack", "prod", "app", "org:stack:prod:app:extra"} {
		_, err := a.requireStackAccess(c, ref)
		var he *echo.HTTPError
		require.ErrorAs(t, err, &he, "%q should not resolve", ref)
		require.Equal(t, http.StatusNotFound, he.Code, "%q", ref)
	}
}

// A path into another tenant is a 404, exactly as its id would be: the gate
// runs on whatever the path resolved to, not on the path.
func TestSlugPathObeysTenancy(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	testdb.SeedStack(t, s, false)
	now := time.Now().UTC()
	other := &repo.Org{ID: "org2", Name: "Other", Slug: "other", CreatedAt: now, SetupDoneAt: &now}
	require.NoError(t, s.CreateOrg(ctx, other))
	require.NoError(t, s.CreateStack(ctx, &repo.Stack{ID: "stack2", OrgID: other.ID,
		Name: "Theirs", Slug: "theirs", CreatedAt: now}))

	a := apiFor(s)
	c := echoCtx(t)
	c.Set(ctxUser, &repo.User{ID: "u1", Role: "member"})
	c.Set(ctxOrgIDs, map[string]bool{"org1": true})

	_, err := a.requireStackAccess(c, "other:theirs")
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he, "a path into another org must 404")
	require.Equal(t, http.StatusNotFound, he.Code)
}

// The first environment builds on push, so it is not a promote target. Saying
// so is the difference between a refused promote and one that quietly
// redeploys the rung that was already there.
func TestPromoteRefusesTheFirstRung(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	rel := service.NewReleaseService(s, nil)

	_, err := rel.Target(ctx, seed.Stack, seed.Env.Slug)
	var invalid svcerr.Invalid
	require.ErrorAs(t, err, &invalid, "the first rung should be refused")

	_, err = rel.Target(ctx, seed.Stack, "nope")
	require.ErrorIs(t, err, svcerr.ErrNotFound)

	now := time.Now().UTC()
	require.NoError(t, s.CreateEnvironment(ctx, &repo.Environment{ID: "env2", StackID: seed.Stack.ID,
		Name: "Staging", Slug: "staging", Type: "static", CreatedAt: now}))
	env, err := rel.Target(ctx, seed.Stack, "staging")
	require.NoError(t, err, "a rung above the first one is a promote target")
	require.Equal(t, "env2", env.ID)
}

// An org needs somebody who can administer it. Removing or demoting the last
// owner would leave it stuck from inside the product: nobody left to invite
// anybody.
func TestLastOwnerCannotBeRemoved(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	members := service.NewMemberService(s, nil, nil)

	now := time.Now().UTC()
	for _, id := range []string{"u1", "u2"} {
		require.NoError(t, s.CreateUser(ctx, &repo.User{ID: id, Email: id + "@test",
			Role: "user", CreatedAt: now}))
	}
	require.NoError(t, s.UpsertOrgMember(ctx, &repo.OrgMember{OrgID: seed.Org.ID,
		UserID: "u1", Role: "owner", CreatedAt: now}))
	err := members.SetRole(ctx, seed.Org, "u1", "viewer")
	var conflict svcerr.Conflict
	require.ErrorAs(t, err, &conflict, "the last owner should be refused")

	require.NoError(t, s.UpsertOrgMember(ctx, &repo.OrgMember{OrgID: seed.Org.ID,
		UserID: "u2", Role: "owner", CreatedAt: now}))
	require.NoError(t, members.SetRole(ctx, seed.Org, "u1", "viewer"),
		"with a second owner the first one can be demoted")
}

// An unknown role is refused, not folded to "member". Folding is why
// `stackr members add --role admin` granted member access and said nothing:
// the caller asked for something the product does not have and was told the
// invite succeeded.
func TestUnknownRoleIsRefused(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	members := service.NewMemberService(s, nil, nil)

	_, err := members.Invite(ctx, seed.Org, "new@test", "administrator", 0, service.Actor{})
	var invalid svcerr.Invalid
	require.ErrorAs(t, err, &invalid, "an unknown role should be refused")

	// And the expiry is bounded on both surfaces: an unbounded one is a
	// credential, a negative one had already expired when it was minted.
	_, err = members.Invite(ctx, seed.Org, "new@test", "member", -1, service.Actor{})
	require.ErrorAs(t, err, &invalid, "a negative expiry should be refused")
	_, err = members.Invite(ctx, seed.Org, "new@test", "member", 10000, service.Actor{})
	require.ErrorAs(t, err, &invalid, "an unbounded expiry should be refused")
}
