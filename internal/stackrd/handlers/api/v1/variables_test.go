package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/orgconf"
	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// call runs a handler with an authenticated admin key carrying scopes.
func call(t *testing.T, a *API, h echo.HandlerFunc, method, target, body, id string, scopes ...string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues(id)
	scopeJSON, _ := json.Marshal(scopes)
	c.Set(ctxKey, &repo.APIKey{Scopes: string(scopeJSON)})
	c.Set(ctxUser, &repo.User{ID: "u1", Role: "admin"})
	return rec, h(c)
}

// callThroughGate is call() with the route's gate in front, for the tests that
// are about what the gate decides — tenancy, the unfinished wizard — rather
// than about what the handler does with a row it was handed.
func callThroughGate(t *testing.T, a *API, h echo.HandlerFunc, method, target, body, id string,
	v service.Verb, k service.Kind, scopes ...string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	return call(t, a, a.gate(v, k, "id", h), method, target, body, id, scopes...)
}

// apiFor builds the API over a real store with no cluster, proxy or engine.
// The tile service is wired because every tile write goes through it now; its
// own dependencies are all nil-safe, so the row is written and the side
// effects are skipped, which is what these tests want to observe.
func apiFor(s *sqlite.Store) *API {
	gate := service.NewGateService(s)
	tiles := service.NewTileService(s, nil, nil, nil, nil, nil, gate)
	envs := service.NewEnvironmentService(s, nil, nil, nil, gate)
	dests := service.NewBackupDestinationService(s, nil)
	return New(s, nil, nil, nil, nil, nil, nil, nil, nil, stackconf.Applier{}).
		WithOrgConfig(&orgconf.Runner{Store: s}).
		WithAccess(service.NewAccessService(s)).
		WithTiles(tiles).
		WithEnvironments(envs).
		WithOrgs(service.NewOrgService(s)).
		WithMembers(service.NewMemberService(s, nil, service.NewRevokeService(s, nil))).
		WithInstances(service.NewManagedInstanceService(s, nil, tiles, gate, nil)).
		WithSlices(service.NewSliceService(s, nil, nil, nil)).
		WithVariables(service.NewVariableService(s, nil, nil, nil)).
		WithLifecycle(service.NewTileLifecycleService(s, nil, nil, nil, nil, nil)).
		WithDomains(service.NewDomainService(s, nil, gate)).
		WithDomainResources(service.NewDomainResourceService(s, nil)).
		WithTelemetry(service.NewTileTelemetryService(s, nil)).
		WithStacks(service.NewStackService(s, nil, nil, envs, nil, gate, nil)).
		WithDeploys(service.NewDeployService(s, nil)).
		WithReleases(service.NewReleaseService(s, nil)).
		WithPlans(service.NewPlanService(s, nil, nil)).
		WithBackupServices(
			service.NewBackupScheduleService(s, dests, nil, gate),
			dests,
		)
}

func decodeVars(t *testing.T, rec *httptest.ResponseRecorder) map[string]varEntry {
	t.Helper()
	var out varsOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "decode %s", rec.Body.String())
	m := map[string]varEntry{}
	for _, v := range out.Vars {
		m[v.Name] = v
	}
	return m
}

// R5: secret values need secrets:read. Without it the name is still listed,
// the name isn't the secret, but the value is masked.
func TestGetVarsMasksSecrets(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	a := apiFor(s)

	body := `{"variables":[{"name":"PORT","value":"8080"},{"name":"TOKEN","value":"s3cr3t","secret":true}]}`
	_, err := call(t, a, a.putVars, http.MethodPut, "/", body, seed.Tile.ID, ScopeVarsWrite)
	require.NoError(t, err, "put")

	rec, err := call(t, a, a.getVars, http.MethodGet, "/", "", seed.Tile.ID, ScopeVarsRead)
	require.NoError(t, err, "get")
	got := decodeVars(t, rec)
	assert.Equal(t, maskedValue, got["TOKEN"].Value, "TOKEN leaked without secrets:read")
	assert.Equal(t, "8080", got["PORT"].Value, "PORT")

	rec, err = call(t, a, a.getVars, http.MethodGet, "/", "", seed.Tile.ID, ScopeVarsRead, ScopeSecretsRead)
	require.NoError(t, err, "get with secrets:read")
	got = decodeVars(t, rec)
	assert.Equal(t, "s3cr3t", got["TOKEN"].Value, "secrets:read did not reveal TOKEN")
}

// secrets:read is a write-grade scope, so it can only be minted by somebody
// with content-write — but the key outlives the role. A member who minted one
// and was then demoted to viewer must stop seeing values at the next request,
// the same way a demoted session does on the panel.
func TestSecretsReadStillNeedsWriteInTheOrg(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	a := apiFor(s)
	ctx := context.Background()
	now := time.Now().UTC()

	body := `{"variables":[{"name":"TOKEN","value":"s3cr3t","secret":true}]}`
	_, err := call(t, a, a.putVars, http.MethodPut, "/", body, seed.Tile.ID, ScopeVarsWrite)
	require.NoError(t, err, "put")

	require.NoError(t, s.CreateUser(ctx, &repo.User{ID: "demoted", Email: "d@example.com",
		Name: "Demoted", Role: "user", Active: true, CreatedAt: now, UpdatedAt: now}), "seed user")
	require.NoError(t, s.UpsertOrgMember(ctx, &repo.OrgMember{
		OrgID: seed.Org.ID, UserID: "demoted", Role: "viewer", CreatedAt: now}), "seed viewer")

	// callAs authenticates as "demoted", a viewer in the tile's own org.
	rec, err := callAs(t, a, a.getVars, http.MethodGet, "/", "", seed.Tile.ID,
		[]string{seed.Org.ID}, ScopeVarsRead, ScopeSecretsRead)
	require.NoError(t, err, "viewer listing variables")
	got := decodeVars(t, rec)
	assert.Equal(t, maskedValue, got["TOKEN"].Value,
		"a viewer's key read a secret value on the strength of a scope alone")
}

// A masked value sent back would silently overwrite the real one with "•••".
func TestPutVarsRejectsMaskedValue(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	a := apiFor(s)

	body := `{"variables":[{"name":"TOKEN","value":"` + maskedValue + `","secret":true}]}`
	_, err := call(t, a, a.putVars, http.MethodPut, "/", body, seed.Tile.ID, ScopeVarsWrite)
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he, "want 400, got %v", err)
	require.Equal(t, http.StatusBadRequest, he.Code, "want 400, got %v", err)
}

// PUT replaces: a variable left out of the payload is gone.
func TestPutVarsReplaces(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	a := apiFor(s)

	first := `{"variables":[{"name":"A","value":"1"},{"name":"B","value":"2"}]}`
	_, err := call(t, a, a.putVars, http.MethodPut, "/", first, seed.Tile.ID, ScopeVarsWrite)
	require.NoError(t, err, "put")
	second := `{"variables":[{"name":"A","value":"9"}]}`
	rec, err := call(t, a, a.putVars, http.MethodPut, "/", second, seed.Tile.ID, ScopeVarsWrite)
	require.NoError(t, err, "put 2")
	got := decodeVars(t, rec)
	require.Len(t, got, 1, "replace left %+v", got)
	require.Equal(t, "9", got["A"].Value, "replace left %+v", got)
}

// B1: the config file owns a managed stack's variables, so an API write must
// 409 rather than be silently reverted by the next plan.
func TestPutVarsBlockedOnManagedStack(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	a := apiFor(s)

	body := `{"variables":[{"name":"A","value":"1"}]}`
	_, err := call(t, a, a.putVars, http.MethodPut, "/", body, seed.Tile.ID, ScopeVarsWrite)
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he, "want 409, got %v", err)
	require.Equal(t, http.StatusConflict, he.Code, "want 409, got %v", err)
}

// Resolved retrieval fails without secrets:read when a secret is involved,
// it must not hand back a partial environment that looks complete.
func TestResolvedVarsNeedsSecretsRead(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	a := apiFor(s)

	body := `{"variables":[{"name":"TOKEN","value":"s3cr3t","secret":true}]}`
	_, err := call(t, a, a.putVars, http.MethodPut, "/", body, seed.Tile.ID, ScopeVarsWrite)
	require.NoError(t, err, "put")
	_, err = call(t, a, a.resolvedVars, http.MethodGet, "/", "", seed.Tile.ID, ScopeVarsRead)
	require.Error(t, err, "resolved returned a secret-bearing env without secrets:read")
	rec, err := call(t, a, a.resolvedVars, http.MethodGet, "/", "", seed.Tile.ID, ScopeVarsRead, ScopeSecretsRead)
	require.NoError(t, err, "resolved with secrets:read")
	got := decodeVars(t, rec)
	assert.Equal(t, "s3cr3t", got["TOKEN"].Value, "resolved TOKEN")
}

// Env variables mirror the stack pair: PUT replaces the environment's own set,
// secret values only come back with secrets:read.
func TestEnvVarsMaskAndReplace(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	a := apiFor(s)

	body := `{"variables":[{"name":"PORT","value":"8080"},{"name":"TOKEN","value":"s3cr3t","secret":true}]}`
	rec, err := call(t, a, a.putEnvVars, http.MethodPut, "/", body, seed.Env.ID, ScopeVarsWrite)
	require.NoError(t, err, "put")
	got := decodeVars(t, rec)
	assert.Equal(t, maskedValue, got["TOKEN"].Value, "put response leaked TOKEN without secrets:read")

	rec, err = call(t, a, a.getEnvVars, http.MethodGet, "/", "", seed.Env.ID, ScopeVarsRead)
	require.NoError(t, err, "get")
	got = decodeVars(t, rec)
	assert.Equal(t, maskedValue, got["TOKEN"].Value, "TOKEN leaked without secrets:read")
	assert.Equal(t, "8080", got["PORT"].Value, "PORT")

	rec, err = call(t, a, a.getEnvVars, http.MethodGet, "/", "", seed.Env.ID, ScopeVarsRead, ScopeSecretsRead)
	require.NoError(t, err, "get with secrets:read")
	got = decodeVars(t, rec)
	assert.Equal(t, "s3cr3t", got["TOKEN"].Value, "secrets:read did not reveal TOKEN")

	// PUT replaces: a variable left out of the payload is gone.
	rec, err = call(t, a, a.putEnvVars, http.MethodPut, "/",
		`{"variables":[{"name":"PORT","value":"9090"}]}`, seed.Env.ID, ScopeVarsWrite)
	require.NoError(t, err, "put 2")
	got = decodeVars(t, rec)
	require.Len(t, got, 1, "replace left %+v", got)
	require.Equal(t, "9090", got["PORT"].Value, "replace left %+v", got)

	// An unknown env id 404s rather than leaking whether it exists.
	_, err = call(t, a, a.getEnvVars, http.MethodGet, "/", "", "nope", ScopeVarsRead)
	require.Error(t, err, "unknown env did not 404")
}

// The catalogue is metadata: it names what can be referenced, never values.
func TestReferenceCatalogueHasNoValues(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	a := apiFor(s)

	_, err := call(t, a, a.putStackVars, http.MethodPut, "/",
		`{"variables":[{"name":"SENTRY_DSN","value":"https://k@sentry/1","secret":true}]}`,
		seed.Stack.ID, ScopeVarsWrite)
	require.NoError(t, err, "stack vars")
	rec, err := call(t, a, a.referenceCatalogue, http.MethodGet, "/", "", seed.Tile.ID, ScopeVarsRead)
	require.NoError(t, err, "catalogue")
	require.NotContains(t, rec.Body.String(), "sentry/1", "catalogue leaked a secret value")
	var got []refSourceOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "decode")
	found := false
	for _, src := range got {
		if src.Scope != "stack" {
			continue
		}
		for _, o := range src.Outputs {
			if o.Name == "SENTRY_DSN" && o.Secret {
				found = true
			}
		}
	}
	assert.True(t, found, "stack variable missing from catalogue: %+v", got)
}
