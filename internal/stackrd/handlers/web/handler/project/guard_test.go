package project

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

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// ctxFor builds what OrgContext would have set: the user, their orgs, and the
// role in the cookie-selected one.
func ctxFor(method, target string, u *repo.User, orgs []repo.Org, role string) echo.Context {
	c := echo.New().NewContext(httptest.NewRequest(method, target, nil), httptest.NewRecorder())
	hamrctx.Set(c, hamrctx.SubjectKey, any(u))
	hamrctx.Set(c, hamrctx.SubjectIDKey, u.ID)
	c.Set(stackrmw.CtxOrgs, orgs)
	if len(orgs) > 0 {
		c.Set(stackrmw.CtxOrg, orgs[0])
	}
	c.Set(stackrmw.CtxRole, role)
	return c
}

func memberOf(t *testing.T, s *sqlite.Store, orgID, userID, role string) *repo.User {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	u := &repo.User{ID: userID, Email: userID + "@example.com", Name: userID, Role: "user", Active: true, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateUser(ctx, u), "create user")
	if role != "" {
		require.NoError(t, s.UpsertOrgMember(ctx, &repo.OrgMember{OrgID: orgID, UserID: u.ID, Role: role, CreatedAt: now}), "member")
	}
	return u
}

// loadStack guarded membership only, so every POST behind it (34 routes: create
// tile, delete env, save vars) ran for an org viewer.
func TestLoadStackRefusesViewerWrites(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	h := NewHandler(s, nil, nil, nil, nil, nil, nil, nil, stackconf.Applier{}, nil)
	u := memberOf(t, s, seed.Org.ID, "viewer1", "viewer")
	orgs := []repo.Org{*seed.Org}

	_, err := h.loadStack(ctxFor(http.MethodPost, "/", u, orgs, "viewer"), seed.Stack.ID)
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he, "viewer POST should be refused")
	assert.Equal(t, http.StatusForbidden, he.Code)

	// The same viewer still reads the stack.
	_, err = h.loadStack(ctxFor(http.MethodGet, "/", u, orgs, "viewer"), seed.Stack.ID)
	assert.NoError(t, err, "viewer GET should still load")
}

// GraphStatus took the env id straight off the URL, so any logged-in user could
// poll any env's tile names, statuses and traffic.
func TestGraphStatusRefusesOutsiders(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	h := NewHandler(s, nil, nil, nil, nil, nil, nil, nil, stackconf.Applier{}, nil)
	u := memberOf(t, s, seed.Org.ID, "outsider", "")

	c := ctxFor(http.MethodGet, "/envs/"+seed.Env.ID+"/graph/status", u, nil, "")
	c.SetParamNames("id")
	c.SetParamValues(seed.Env.ID)

	var he *echo.HTTPError
	require.ErrorAs(t, h.GraphStatus(c), &he, "outsider should be refused")
	assert.Equal(t, http.StatusNotFound, he.Code)
}
