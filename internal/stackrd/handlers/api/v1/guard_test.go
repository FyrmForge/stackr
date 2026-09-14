package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

// managedGuard is the discriminator behind the config-managed write lock: a
// stack bound to a config repo owns its structure, so structural writes must be
// rejected (a plan would revert them). Everything else passes through.
func TestManagedGuard(t *testing.T) {
	cases := []struct {
		name    string
		stack   *repo.Stack
		want409 bool
	}{
		{"nil stack passes", nil, false},
		{"ui-managed passes", &repo.Stack{}, false},
		{"only connector, no repo passes", &repo.Stack{ConfigConnectorID: "c1"}, false},
		{"config-managed rejected", &repo.Stack{ConfigConnectorID: "c1", ConfigRepo: "org/cfg"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := managedGuard(tc.stack)
			if !tc.want409 {
				require.NoError(t, err, "want nil")
				return
			}
			var he *echo.HTTPError
			require.ErrorAs(t, err, &he, "want 409 HTTPError, got %v", err)
			require.Equal(t, http.StatusConflict, he.Code, "want 409 HTTPError, got %v", err)
		})
	}
}

// The wizard closed the panel's settings and nothing else, so /api/v1 drove a
// half-set-up org around the outside, which is the whole org, because the CLI
// speaks nothing but this. It refuses with a 409 rather than a redirect: the
// wizard is HTML, and the CLI would render it as a parse error.
func TestAPIRefusesUnfinishedOrg(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	a := apiFor(s)

	seed.Org.SetupDoneAt = nil
	require.NoError(t, s.UpdateOrg(t.Context(), seed.Org), "reopen the wizard")

	_, err := call(t, a, a.getVars, http.MethodGet, "/", "", seed.Tile.ID, ScopeVarsRead)
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he, "want 409, got %v", err)
	require.Equal(t, http.StatusConflict, he.Code, "want 409, got %v", err)
	require.Contains(t, he.Message, "/orgs/org/setup/done", "the refusal has to say where to finish it")

	// A listing filters it out instead: nothing to tell a caller who asked
	// about everything, and a 409 there would hide the orgs that do work.
	rec, err := call(t, a, a.listApps, http.MethodGet, "/", "", "", ScopeAppsRead)
	require.NoError(t, err, "list")
	require.Equal(t, "[]\n", rec.Body.String(), "an unfinished org's tiles were listed")
}

// The brand-new user: one org, still in the wizard. Dropping unfinished orgs
// from the candidate set (so a user with one working org and one draft is not
// asked to name an org_id) hid this case behind "no organization with write
// access", which is false, they own it, and it says nothing about the wizard.
func TestCreateInOnlyOrgSaysFinishSetup(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	a := apiFor(s)
	ctx := t.Context()

	seed.Org.SetupDoneAt = nil
	require.NoError(t, s.UpdateOrg(ctx, seed.Org), "reopen the wizard")
	u := &repo.User{ID: "u1", Email: "u@example.com", Name: "U", Role: "user", Active: true}
	require.NoError(t, s.CreateUser(ctx, u))
	require.NoError(t, s.UpsertOrgMember(ctx, &repo.OrgMember{OrgID: seed.Org.ID, UserID: u.ID, Role: "owner"}))

	c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", nil), httptest.NewRecorder())
	c.Set(ctxUser, u)
	c.Set(ctxOrgIDs, map[string]bool{seed.Org.ID: true})

	_, err := a.orgForCreate(c, "")
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he, "want 409, got %v", err)
	require.Equal(t, http.StatusConflict, he.Code, "want 409, got %v", err)
	require.Contains(t, he.Message, "/orgs/org/setup/done", "the refusal has to say where to finish it")
}
