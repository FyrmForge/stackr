package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	hamrctx "github.com/FyrmForge/hamr/pkg/ctx"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// asViewer builds the request context OrgContext would have built for a user
// whose role in the active org is viewer.
func asViewer(method, path string) echo.Context {
	now := time.Now().UTC()
	u := &repo.User{ID: "u1", Email: "v@example.com", Role: "user", Active: true}
	org := repo.Org{ID: "org1", Name: "O", Slug: "o", CreatedAt: now}
	c := echo.New().NewContext(httptest.NewRequest(method, path, nil), httptest.NewRecorder())
	hamrctx.Set(c, hamrctx.SubjectKey, any(u))
	hamrctx.Set(c, hamrctx.SubjectIDKey, u.ID)
	c.Set(stackrmw.CtxOrgs, []repo.Org{org})
	c.Set(stackrmw.CtxOrg, org)
	c.Set(stackrmw.CtxRole, "viewer")
	return c
}

// The guard used to refuse every non-GET for a viewer, which locked
// them out of their own password, API keys, preferences and notifications.
// Those rows are the user's, not the org's.
func TestReadOnlyGuardLetsViewersEditTheirOwnAccount(t *testing.T) {
	ok := func(c echo.Context) error { return c.NoContent(http.StatusOK) }
	h := stackrmw.ReadOnlyGuard()(ok)

	self := []string{
		"/account/password", "/account/profile", "/account/appearance",
		"/account/notifications", "/account/apikeys", "/account/apikeys/k1/delete",
		"/account/graph-prefs", "/notifications/read", "/notifications/clear",
	}
	for _, p := range self {
		require.NoError(t, h(asViewer(http.MethodPost, p)), "viewer must be able to POST %s", p)
	}

	// Org content is still refused.
	for _, p := range []string{"/orgs/o/settings/domains", "/envs/e1/vars/delete", "/account"} {
		err := h(asViewer(http.MethodPost, p))
		require.Error(t, err, "viewer must not be able to POST %s", p)
		require.Equal(t, http.StatusForbidden, err.(*echo.HTTPError).Code)
	}
}
