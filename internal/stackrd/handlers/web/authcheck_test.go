package web

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

// Traefik asks this endpoint about every request to a protected preview
// hostname. Answering on "is there a session" alone made every organization's
// preview URLs readable by anyone with a login, which is the opposite of what
// the setting promises.
func TestAuthCheck(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	ctx := context.Background()
	now := time.Now().UTC()

	other := &repo.Org{ID: "org2", Name: "Other", Slug: "other", CreatedAt: now, SetupDoneAt: &now}
	require.NoError(t, store.CreateOrg(ctx, other), "create org2")
	dom := &repo.Domain{ID: "d1", TileID: seed.Tile.ID, Host: "app-dev-stack.example.com",
		Path: "/", ContainerPort: 80, HTTPS: true, Auto: true, CreatedAt: now}
	require.NoError(t, store.CreateDomain(ctx, dom), "create domain")

	h := authCheck(store, "https://panel.example.com")
	call := func(host string, u *repo.User, orgs []repo.Org) (int, error) {
		req := httptest.NewRequest(http.MethodGet, "/_stackr/authcheck", nil)
		req.Header.Set("X-Forwarded-Host", host)
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(req, rec)
		if u != nil {
			hamrctx.Set(c, hamrctx.SubjectKey, any(u))
			hamrctx.Set(c, hamrctx.SubjectIDKey, u.ID)
			c.Set(stackrmw.CtxOrgs, orgs)
		}
		return rec.Code, h(c)
	}

	member := &repo.User{ID: "u1", Role: "user"}
	outsider := &repo.User{ID: "u2", Role: "user"}
	admin := &repo.User{ID: "u3", Role: "admin"}

	t.Run("signed out redirects to login", func(t *testing.T) {
		code, err := call(dom.Host, nil, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusSeeOther, code)
	})
	t.Run("member of the owning org passes", func(t *testing.T) {
		code, err := call(dom.Host, member, []repo.Org{*seed.Org})
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, code)
	})
	t.Run("host with a port still resolves", func(t *testing.T) {
		code, err := call(dom.Host+":8443", member, []repo.Org{*seed.Org})
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, code)
	})
	t.Run("member of another org is refused", func(t *testing.T) {
		_, err := call(dom.Host, outsider, []repo.Org{*other})
		he, ok := err.(*echo.HTTPError)
		require.True(t, ok && he.Code == http.StatusForbidden, "want 403, got %v", err)
	})
	t.Run("admin passes", func(t *testing.T) {
		code, err := call(dom.Host, admin, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, code)
	})
	t.Run("unknown host is refused, not waved through", func(t *testing.T) {
		_, err := call("someone-elses.example.com", member, []repo.Org{*seed.Org})
		he, ok := err.(*echo.HTTPError)
		require.True(t, ok && he.Code == http.StatusForbidden, "want 403, got %v", err)
	})
}
