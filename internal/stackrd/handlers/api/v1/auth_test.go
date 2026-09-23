package v1

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func TestHashKey(t *testing.T) {
	// SHA-256 of the empty string, a known vector; proves the hash is real sha256.
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", HashKey(""), `HashKey("")`)
	assert.NotEqual(t, HashKey("a"), HashKey("b"), "distinct inputs hashed to the same value")
}

func TestValidScope(t *testing.T) {
	assert.True(t, ValidScope(ScopeAppsRead), "%q should be valid", ScopeAppsRead)
	assert.False(t, ValidScope("apps:teleport"), "unknown scope reported valid")
}

func TestWriteScope(t *testing.T) {
	assert.False(t, WriteScope(ScopeStacksRead), "%q is a read scope, should not require write", ScopeStacksRead)
	assert.True(t, WriteScope(ScopeStacksWrite), "%q should require write", ScopeStacksWrite)
	assert.True(t, WriteScope("unknown:scope"), "unknown scope must default to privileged (write-required)")
}

// TestScopeCatalogConsistent guards the catalog against drift: every entry must
// validate, and WriteScope must agree with its Write flag.
func TestScopeCatalogConsistent(t *testing.T) {
	for _, sc := range Scopes {
		assert.True(t, ValidScope(sc.Name), "catalog scope %q fails ValidScope", sc.Name)
		assert.Equal(t, sc.Write, WriteScope(sc.Name), "scope %q: WriteScope disagrees with catalog Write", sc.Name)
	}
}

func TestAPIKeyHasScope(t *testing.T) {
	k := &repo.APIKey{Scopes: `["apps:read","logs:read"]`}
	assert.True(t, k.HasScope("apps:read"), "granted scope reported missing")
	assert.False(t, k.HasScope("apps:write"), "ungranted scope reported present")
	assert.Len(t, k.ScopeList(), 2, "ScopeList = %v, want 2 entries", k.ScopeList())
	empty := &repo.APIKey{Scopes: ""}
	assert.False(t, empty.HasScope("apps:read"), "empty scopes should grant nothing")
	assert.Len(t, empty.ScopeList(), 0, "empty scopes should grant nothing")
}

// A key minted in one org carries its scopes nowhere else. Unbound keys —
// every key that existed before the org column — are untouched, which is the
// whole point of not making the binding retroactive.
func TestKeyOrgAllows(t *testing.T) {
	a := &API{}
	cases := []struct {
		name   string
		key    *repo.APIKey
		target string
		ok     bool
	}{
		{"unbound key goes anywhere", &repo.APIKey{}, "o2", true},
		{"bound key in its own org", &repo.APIKey{OrgID: sql.NullString{String: "o1", Valid: true}}, "o1", true},
		{"bound key in another org", &repo.APIKey{OrgID: sql.NullString{String: "o1", Valid: true}}, "o2", false},
		{"server-wide route has no org to bind", &repo.APIKey{OrgID: sql.NullString{String: "o1", Valid: true}}, "", true},
		{"session, not a key", nil, "o2", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
			if tc.key != nil {
				c.Set(ctxKey, tc.key)
			}
			err := a.keyOrgAllows(c, tc.target)
			if tc.ok {
				assert.NoError(t, err)
				return
			}
			// 404, not 403: a key that may not act here does not learn the
			// org exists.
			var he *echo.HTTPError
			require.ErrorAs(t, err, &he)
			assert.Equal(t, http.StatusNotFound, he.Code)
		})
	}
}
