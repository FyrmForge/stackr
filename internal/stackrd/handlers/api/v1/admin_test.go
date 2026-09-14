package v1

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Storage and proxy config are panel-wide, not org-scoped, and the web pages
// for them are admin-only. The API routes carried a scope check and nothing
// else, and any user can create an org, become its owner and mint a key with
// any scope, so the scope alone let any user read and rewrite them.
func TestAdminOnlyRefusesNonAdminKeys(t *testing.T) {
	a := apiFor(testdb.New(t))

	for name, h := range map[string]echo.HandlerFunc{
		"listStorage":    a.listStorage,
		"createStorage":  a.createStorage,
		"deleteStorage":  a.deleteStorage,
		"getProxyConfig": a.getProxyConfig,
		"putProxyEntry":  a.putProxyEntry,
	} {
		// 403 lands before the handler body, so the nil-dep API is fine here.
		_, err := callAs(t, a, a.adminOnly(h), http.MethodGet, "/", "", "x", nil,
			ScopeStorageRead, ScopeStorageWrite, ScopeDomainsWrite)
		var he *echo.HTTPError
		require.ErrorAs(t, err, &he, "%s: want an HTTP error", name)
		assert.Equal(t, http.StatusForbidden, he.Code, "%s", name)
	}
}

func TestAdminOnlyLetsAdminsThrough(t *testing.T) {
	a := apiFor(testdb.New(t))
	ran := false
	h := a.adminOnly(func(c echo.Context) error {
		ran = true
		return c.NoContent(http.StatusOK)
	})
	rec, err := call(t, a, h, http.MethodGet, "/", "", "x", ScopeStorageRead)
	require.NoError(t, err)
	assert.True(t, ran, "handler should run for an admin key")
	assert.Equal(t, http.StatusOK, rec.Code)
}
