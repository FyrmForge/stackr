package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	hamrctx "github.com/FyrmForge/hamr/pkg/ctx"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// An unfinished draft is gated everywhere but its own wizard, so holding it as
// the active org means every page bounces. Creating a draft also writes the
// org cookie, and that cookie outlives the session: a draft abandoned
// mid-wizard was still active after a fresh login (QA 2026-09-17 BUG-5, and
// again 2026-09-18 R2-2 when only the fallback had been fixed).
func TestOrgContextPrefersAFinishedOrg(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	now := time.Now().UTC()

	// The draft is created first, so it wins the naive "first org" pick.
	draft := &repo.Org{ID: "o-draft", Name: "Untitled organization", Slug: "org-abc123", CreatedAt: now}
	require.NoError(t, store.CreateOrg(ctx, draft))
	finished := &repo.Org{ID: "o-done", Name: "Acme", Slug: "acme", CreatedAt: now.Add(time.Minute), SetupDoneAt: &now}
	require.NoError(t, store.CreateOrg(ctx, finished))

	admin := &repo.User{ID: "u-admin", Email: "a@example.com", Name: "Admin", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateUser(ctx, admin))

	activeFor := func(u *repo.User, cookie string) repo.Org {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: stackrmw.OrgCookie, Value: cookie})
		}
		c := echo.New().NewContext(req, httptest.NewRecorder())
		hamrctx.Set(c, hamrctx.SubjectKey, any(u))
		var got repo.Org
		h := stackrmw.OrgContext(store)(func(c echo.Context) error {
			o, ok := stackrmw.ActiveOrg(c)
			require.True(t, ok)
			got = o
			return nil
		})
		require.NoError(t, h(c))
		return got
	}

	require.Equal(t, finished.ID, activeFor(admin, "").ID, "no cookie: a draft must not win over a finished org")
	require.Equal(t, finished.ID, activeFor(admin, draft.ID).ID, "a stale cookie naming a draft must not win either")
	require.Equal(t, finished.ID, activeFor(admin, finished.ID).ID, "a cookie naming a finished org still wins")

	// A non-admin owner's list comes from ListOrgsForUser, a different branch
	// of the same middleware, so the guard has to hold there too.
	owner := &repo.User{ID: "u-owner", Email: "o@example.com", Name: "Owner", Role: "user", Active: true, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateUser(ctx, owner))
	for _, id := range []string{draft.ID, finished.ID} {
		require.NoError(t, store.UpsertOrgMember(ctx, &repo.OrgMember{OrgID: id, UserID: owner.ID, Role: "owner", CreatedAt: now}))
	}
	require.Equal(t, finished.ID, activeFor(owner, draft.ID).ID, "same guard on the member-scoped list")

	// With nothing finished the draft is all there is, and someone mid-wizard
	// needs it: refusing here would leave the wizard with no active org at all.
	require.NoError(t, store.DeleteOrg(ctx, finished.ID))
	require.Equal(t, draft.ID, activeFor(admin, "").ID, "every org is a draft, so the draft is the active one")
}
