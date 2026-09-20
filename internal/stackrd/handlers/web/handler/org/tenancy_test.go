package org

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	hamrctx "github.com/FyrmForge/hamr/pkg/ctx"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Org-addressed writes must answer for the org in the URL, not the one the
// cookie selects. ReadOnlyGuard, the only write check these routes sit behind,
// reads the active org's role, so a user who owns one org and merely views
// another used to be able to move that org's stacks and rewrite its shared
// canvas layout. Same hole tenancy_test.go closed for stack-addressed routes.

// asUser builds the context OrgContext would have: the user, their orgs, and
// the role in the active (first) one.
func asUser(t *testing.T, method, body string, u *repo.User, orgs []repo.Org, activeRole string) echo.Context {
	t.Helper()
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	if strings.HasPrefix(body, "{") {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	} else if body != "" {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	}
	c := echo.New().NewContext(req, httptest.NewRecorder())
	hamrctx.Set(c, hamrctx.SubjectKey, any(u))
	hamrctx.Set(c, hamrctx.SubjectIDKey, u.ID)
	c.Set(stackrmw.CtxOrgs, orgs)
	c.Set(stackrmw.CtxOrg, orgs[0])
	c.Set(stackrmw.CtxRole, activeRole)
	return c
}

// ownerHereViewerThere: owner of the seeded org (the active one), viewer of a
// second org that owns its own stack.
func ownerHereViewerThere(t *testing.T, s *sqlite.Store) (*repo.User, []repo.Org, *repo.Org, *repo.Stack, *repo.Stack) {
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
	return u, []repo.Org{*seed.Org, *other}, other, otherStack, seed.Stack
}

// gated runs a handler the way the router does: behind the route's verb gate,
// which is where these checks live now. Calling the handler bare asserts
// nothing about who is asking — that is what moving authorization to the route
// means — so a test that does is testing the loader, not the rule.
func gated(s *sqlite.Store, v service.Verb, k service.Kind, param string, h echo.HandlerFunc) echo.HandlerFunc {
	return stackrmw.Gate(s, service.NewAccessService(s), v, k, param)(h)
}

func wantStatus(t *testing.T, what string, err error, code int) {
	t.Helper()
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok && he.Code == code, "%s: want %d, got %v", what, code, err)
}

func TestOrgCanvasWritesCheckThatOrgsRole(t *testing.T) {
	s := testdb.New(t)
	u, orgs, other, _, _ := ownerHereViewerThere(t, s)
	h := NewHandler(s, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).
		WithDomainResources(service.NewDomainResourceService(s, nil)).
		WithPlans(service.NewPlanService(s, nil, nil)).
		WithGraph(service.NewGraphService(s))

	// Reading the canvas of an org they only view stays allowed.
	c := asUser(t, http.MethodGet, "", u, orgs, "owner")
	c.SetParamNames("id")
	c.SetParamValues(other.ID)
	_, err := h.loadOrg(c)
	require.NoError(t, err, "viewer reading another org's canvas")

	// Dragging a card there is a write to shared state: refused, even though
	// the active-org role says owner.
	c = asUser(t, http.MethodPost, `{"node_id":"stack:s2","x":10,"y":20}`, u, orgs, "owner")
	c.SetParamNames("id")
	c.SetParamValues(other.ID)
	wantStatus(t, "viewer saving positions",
		gated(s, service.VerbOrgGraphWrite, service.KindOrg, "id", h.SaveNodePosition)(c),
		http.StatusForbidden)

	c = asUser(t, http.MethodPost, "", u, orgs, "owner")
	c.SetParamNames("id")
	c.SetParamValues(other.ID)
	wantStatus(t, "viewer resetting positions",
		gated(s, service.VerbOrgGraphWrite, service.KindOrg, "id", h.ResetNodePositions)(c),
		http.StatusForbidden)

	// In the org they own, the same write goes through.
	c = asUser(t, http.MethodPost, `{"node_id":"stack:s1","x":10,"y":20}`, u, orgs, "owner")
	c.SetParamNames("id")
	c.SetParamValues(orgs[0].ID)
	require.NoError(t, gated(s, service.VerbOrgGraphWrite, service.KindOrg, "id", h.SaveNodePosition)(c),
		"owner saving positions in their own org")
}

// A move edits both orgs' contents, so write rights in either one alone is not
// enough.
func TestMoveStackChecksBothOrgs(t *testing.T) {
	s := testdb.New(t)
	u, orgs, other, otherStack, ownStack := ownerHereViewerThere(t, s)
	h := NewHandler(s, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).
		WithDomainResources(service.NewDomainResourceService(s, nil)).
		WithPlans(service.NewPlanService(s, nil, nil)).
		WithGraph(service.NewGraphService(s))
	owned := orgs[0]

	// Pulling a stack out of the org they only view.
	c := asUser(t, http.MethodPost, "org_id="+owned.ID, u, orgs, "owner")
	c.SetParamNames("id")
	c.SetParamValues(otherStack.ID)
	wantStatus(t, "moving another org's stack out",
		gated(s, service.VerbStackWrite, service.KindStack, "id", h.MoveStack)(c),
		http.StatusForbidden)

	// Pushing one of their own into it.
	c = asUser(t, http.MethodPost, "org_id="+other.ID, u, orgs, "owner")
	c.SetParamNames("id")
	c.SetParamValues(ownStack.ID)
	// The gate answers for the org the stack is LEAVING; the org it is moving
	// INTO arrives in the body, so that half stays in the handler.
	wantStatus(t, "moving a stack into an org they only view",
		gated(s, service.VerbStackWrite, service.KindStack, "id", h.MoveStack)(c),
		http.StatusForbidden)
}
