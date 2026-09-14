package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	hamrctx "github.com/FyrmForge/hamr/pkg/ctx"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// asUser builds a request context the way OrgContext would have: the user, the
// orgs they belong to, and the role in the *active* (cookie-selected) one.
func asUser(method string, u *repo.User, orgs []repo.Org, activeRole string) echo.Context {
	c := echo.New().NewContext(httptest.NewRequest(method, "/", nil), httptest.NewRecorder())
	hamrctx.Set(c, hamrctx.SubjectKey, any(u))
	hamrctx.Set(c, hamrctx.SubjectIDKey, u.ID)
	c.Set(stackrmw.CtxOrgs, orgs)
	if len(orgs) > 0 {
		c.Set(stackrmw.CtxOrg, orgs[0])
	}
	c.Set(stackrmw.CtxRole, activeRole)
	return c
}

// twoOrgUser: owner of org1 (which the cookie selects), viewer of org2.
func twoOrgUser(t *testing.T, s *sqlite.Store) (*repo.User, []repo.Org, testdb.Seed, *repo.Stack) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	seed := testdb.SeedStack(t, s, false)

	u := &repo.User{ID: "u1", Email: "u@example.com", Name: "U", Role: "user", Active: true, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateUser(ctx, u), "create user")
	other := &repo.Org{ID: "org2", Name: "Other", Slug: "other", CreatedAt: now, SetupDoneAt: &now}
	require.NoError(t, s.CreateOrg(ctx, other), "create org2")
	otherStack := &repo.Stack{ID: "stack2", OrgID: other.ID, Name: "S2", Slug: "s2", CreatedAt: now}
	require.NoError(t, s.CreateStack(ctx, otherStack), "create stack2")
	for _, m := range []*repo.OrgMember{
		{OrgID: seed.Org.ID, UserID: u.ID, Role: "owner", CreatedAt: now},
		{OrgID: other.ID, UserID: u.ID, Role: "viewer", CreatedAt: now},
	} {
		require.NoError(t, s.UpsertOrgMember(ctx, m), "member")
	}
	return u, []repo.Org{*seed.Org, *other}, seed, otherStack
}

// The role that matters is the one in the org owning the resource, not the one
// in whichever org the cookie happens to select. ReadOnlyGuard can only check
// the latter, so an owner-here/viewer-there user used to sail through every
// id-addressed write, volume file deletes, table rows, deploys.
func TestStackWriteChecksTheStacksOwnOrg(t *testing.T) {
	s := testdb.New(t)
	u, orgs, seed, otherStack := twoOrgUser(t, s)

	// Writing in the org they own: allowed.
	require.NoError(t, stackrmw.RequireStackAccess(asUser(http.MethodPost, u, orgs, "owner"), s, seed.Stack.ID),
		"owner writing their own org")
	// Reading the org they only view: allowed.
	require.NoError(t, stackrmw.RequireStackAccess(asUser(http.MethodGet, u, orgs, "owner"), s, otherStack.ID),
		"viewer reading")
	// Writing there: refused, even though the active-org role says owner.
	err := stackrmw.RequireStackAccess(asUser(http.MethodPost, u, orgs, "owner"), s, otherStack.ID)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok, "viewer writing in another org: want 403, got %v", err)
	require.Equal(t, http.StatusForbidden, he.Code, "viewer writing in another org: want 403, got %v", err)
}

// The flag that decides whether upload and delete buttons render has to answer
// for the resource's org too, or a viewer is shown controls that 403.
func TestCanWriteHereFollowsTheResource(t *testing.T) {
	s := testdb.New(t)
	u, orgs, _, otherStack := twoOrgUser(t, s)

	c := asUser(http.MethodGet, u, orgs, "owner")
	assert.True(t, stackrmw.CanWriteHere(c), "before any access check, CanWriteHere should fall back to the active org (owner)")
	require.NoError(t, stackrmw.RequireStackAccess(c, s, otherStack.ID), "read access")
	assert.False(t, stackrmw.CanWriteHere(c), "after resolving a stack they only view, CanWriteHere still said yes")
}
