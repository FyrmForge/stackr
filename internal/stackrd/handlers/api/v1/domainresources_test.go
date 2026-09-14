package v1

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Create + list + delete round-trip at stack level, plus the duplicate-host
// conflict the unique index backs.
func TestDomainResourcesCRUD(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	a := apiFor(s)

	body := `{"host":"example.com","level":"stack","owner":"` + seed.Stack.ID + `","include_env_on_default":true}`
	rec, err := call(t, a, a.createDomainResource, http.MethodPost, "/", body, "", ScopeDomainsWrite)
	require.NoError(t, err, "create")
	var created domainResourceOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created), "decode")
	require.True(t, created.Level == "stack" && created.OwnerID == seed.Stack.ID && created.IncludeEnvOnDefault,
		"created = %+v", created)

	// Same host again → 409, before the DB index has to say so.
	_, err = call(t, a, a.createDomainResource, http.MethodPost, "/", body, "", ScopeDomainsWrite)
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he, "duplicate host: want 409, got %v", err)
	require.Equal(t, http.StatusConflict, he.Code, "duplicate host: want 409, got %v", err)

	rec, err = call(t, a, a.listDomainResources, http.MethodGet, "/", "", "", ScopeStacksRead)
	require.NoError(t, err, "list")
	var listed []domainResourceOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed), "decode list")
	require.Len(t, listed, 1, "list = %+v", listed)
	require.Equal(t, "example.com", listed[0].Host, "list = %+v", listed)

	_, err = call(t, a, a.deleteDomainResource, http.MethodDelete, "/", "", created.ID, ScopeDomainsWrite)
	require.NoError(t, err, "delete")
	_, err = call(t, a, a.deleteDomainResource, http.MethodDelete, "/", "", created.ID, ScopeDomainsWrite)
	require.Error(t, err, "second delete did not 404")
}

// Instance level is admin-only, host syntax is validated, and other tenants'
// org rows neither list nor delete.
func TestDomainResourcesTenancy(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	a := apiFor(s)

	// Non-admin cannot claim the instance level.
	_, err := callAs(t, a, a.createDomainResource, http.MethodPost, "/", `{"host":"inst.dev","level":"instance"}`, "",
		[]string{seed.Org.ID}, ScopeDomainsWrite)
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he, "instance as member: want 403, got %v", err)
	require.Equal(t, http.StatusForbidden, he.Code, "instance as member: want 403, got %v", err)

	// A host with a scheme is refused.
	_, err = call(t, a, a.createDomainResource, http.MethodPost, "/", `{"host":"https://x.dev","level":"instance"}`, "", ScopeDomainsWrite)
	require.ErrorAs(t, err, &he, "scheme host: want 400, got %v", err)
	require.Equal(t, http.StatusBadRequest, he.Code, "scheme host: want 400, got %v", err)

	// Admin creates an org-level row; a user outside that org can't see or
	// delete it (404, not 403, ids don't leak across tenants).
	rec, err := call(t, a, a.createDomainResource, http.MethodPost, "/",
		`{"host":"org.dev","level":"org","owner":"`+seed.Org.ID+`"}`, "", ScopeDomainsWrite)
	require.NoError(t, err, "create org row")
	var created domainResourceOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created), "decode")
	rec, err = callAs(t, a, a.listDomainResources, http.MethodGet, "/", "", "", []string{"other-org"}, ScopeStacksRead)
	require.NoError(t, err, "list as outsider")
	var listed []domainResourceOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed), "decode list")
	require.Len(t, listed, 0, "outsider sees %+v", listed)
	_, err = callAs(t, a, a.deleteDomainResource, http.MethodDelete, "/", "", created.ID, []string{"other-org"}, ScopeDomainsWrite)
	require.ErrorAs(t, err, &he, "outsider delete: want 404, got %v", err)
	require.Equal(t, http.StatusNotFound, he.Code, "outsider delete: want 404, got %v", err)
}
