package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Approving a plan applies whatever the config file says, deletes included, on
// an environment whose apply policy is deliberately manual. The API's default
// gate is the key's scope alone, no ReadOnlyGuard equivalent to the web's,
// so these tests pin that the decision routes also demand a live org write
// role, and that a plan is unreachable from another tenant.

const planBlob = `{"changes":[{"kind":"delete","env":"prod","tile":"api"},` +
	`{"kind":"update","env":"prod","tile":"web","field":"image","old":"a","new":"b"}]}`

func seedPlan(t *testing.T, s *sqlite.Store, stackID, status string) *repo.ConfigPlan {
	t.Helper()
	cp := &repo.ConfigPlan{ID: "plan-" + stackID + "-" + status, StackID: stackID,
		Status: status, Summary: "2 changes", Plan: planBlob, CreatedAt: time.Now().UTC()}
	require.NoError(t, s.CreateConfigPlan(context.Background(), cp), "create plan")
	return cp
}

// A member of the stack's own org, but read-only, holding config:apply: the
// scope is not the authority, the role is.
func TestApproveRejectRequireOrgWrite(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	cp := seedPlan(t, s, seed.Stack.ID, "pending")
	a := apiFor(s)
	orgs := []string{seed.Org.ID}

	// callAs authenticates as "outsider"; give them a real read-only seat in
	// the stack's own org, so the 403 comes from the role and not from absence.
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, s.CreateUser(ctx, &repo.User{ID: "outsider", Email: "v@example.com",
		Name: "Viewer", Role: "user", Active: true, CreatedAt: now, UpdatedAt: now}), "seed user")
	require.NoError(t, s.UpsertOrgMember(ctx, &repo.OrgMember{
		OrgID: seed.Org.ID, UserID: "outsider", Role: "viewer", CreatedAt: now}), "seed viewer")

	_, err := callGated(t, a, a.approvePlan, http.MethodPost, "/", "", cp.ID, orgs,
		service.VerbStackPlanApprove, service.KindStackPlan, ScopeConfigApply)
	wantStatus(t, err, http.StatusForbidden, "viewer approving")

	_, err = callGated(t, a, a.rejectPlan, http.MethodPost, "/", "", cp.ID, orgs,
		service.VerbStackPlanApprove, service.KindStackPlan, ScopeConfigApply)
	wantStatus(t, err, http.StatusForbidden, "viewer rejecting")

	// Reading is a different question, a viewer may see the plan.
	rec, err := callGated(t, a, a.getPlan, http.MethodGet, "/", "", cp.ID, orgs,
		service.VerbOrgRead, service.KindStackPlan, ScopeConfigRead)
	require.NoError(t, err, "viewer reading plan")
	require.Equal(t, http.StatusOK, rec.Code, "viewer reading plan")
}

// Plan ids are opaque; a member of another org must get 404, not 403, so the
// id's existence doesn't leak.
func TestConfigPlanRoutesRejectOtherOrg(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	cp := seedPlan(t, s, seed.Stack.ID, "pending")
	a := apiFor(s)
	outsider := []string{"org2"}

	_, err := callGated(t, a, a.getPlan, http.MethodGet, "/", "", cp.ID, outsider,
		service.VerbOrgRead, service.KindStackPlan, ScopeConfigRead)
	wantStatus(t, err, http.StatusNotFound, "outsider reading plan")

	_, err = callGated(t, a, a.approvePlan, http.MethodPost, "/", "", cp.ID, outsider,
		service.VerbStackPlanApprove, service.KindStackPlan, ScopeConfigApply)
	wantStatus(t, err, http.StatusNotFound, "outsider approving plan")

	_, err = callGated(t, a, a.listPlans, http.MethodGet, "/", "", seed.Stack.ID, outsider,
		service.VerbOrgRead, service.KindStack, ScopeConfigRead)
	wantStatus(t, err, http.StatusNotFound, "outsider listing plans")
}

// A decided plan must not be re-decided: approving an already-applied plan
// would re-run an apply nobody asked for.
func TestDecideRejectsNonPendingPlan(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	cp := seedPlan(t, s, seed.Stack.ID, "applied")
	a := apiFor(s)

	_, err := call(t, a, a.approvePlan, http.MethodPost, "/", "", cp.ID, ScopeConfigApply)
	wantStatus(t, err, http.StatusConflict, "approving an applied plan")

	_, err = call(t, a, a.rejectPlan, http.MethodPost, "/", "", cp.ID, ScopeConfigApply)
	wantStatus(t, err, http.StatusConflict, "rejecting an applied plan")
}

// Rejecting is the one decision that touches no infrastructure, so it runs end
// to end here without a live applier.
func TestRejectPlanMarksRejected(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	cp := seedPlan(t, s, seed.Stack.ID, "pending")
	a := apiFor(s)

	rec, err := call(t, a, a.rejectPlan, http.MethodPost, "/", "", cp.ID, ScopeConfigApply)
	require.NoError(t, err, "reject")
	var got planDetailOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "decode")
	assert.Equal(t, "rejected", got.Status, "response status")
	// Destructive is what a CI gate keys on, and the blob has a delete in it.
	assert.True(t, got.Destructive && len(got.Changes) == 2,
		"changes = %d destructive = %v, want 2 and true", len(got.Changes), got.Destructive)
	fresh, ferr := s.GetConfigPlan(context.Background(), cp.ID)
	assert.NoError(t, ferr, "stored status = %v (%v), want rejected", fresh, ferr)
	if ferr == nil {
		assert.Equal(t, "rejected", fresh.Status, "stored status = %v, want rejected", fresh)
	}
}

// A stack with no config repo bound has no plans to speak of; saying so beats
// returning an empty list that looks like "nothing to apply".
func TestConfigRoutesRejectUnmanagedStack(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false) // not config-managed
	a := apiFor(s)

	_, err := call(t, a, a.planStack, http.MethodPost, "/", "", seed.Stack.ID, ScopeConfigApply)
	wantStatus(t, err, http.StatusConflict, "planning an unmanaged stack")

	// Reading is not blocked by the binding: unbinding a repo does not erase
	// the plans applied under it, and the history has to stay readable.
	rec, err := call(t, a, a.listPlans, http.MethodGet, "/", "", seed.Stack.ID, ScopeConfigRead)
	require.NoError(t, err, "listing plans on an unmanaged stack")
	require.Equal(t, http.StatusOK, rec.Code, "listing plans on an unmanaged stack")
}

// Preview computes a plan from a posted bundle and must store nothing, it is
// the config:read dry-run; a row here would show up as pending work. The
// bundle's include has to resolve from the posted files, not from git.
func TestPreviewPlanComputesWithoutStoring(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	// Pin the branch so StackBranch resolves locally, no git source in tests.
	seed.Stack.ConfigBranch = "main"
	require.NoError(t, s.UpdateStack(context.Background(), seed.Stack), "pin branch")
	a := apiFor(s)
	a.applier = stackconf.Applier{Planner: stackconf.Planner{Store: s}}

	body, _ := json.Marshal(map[string]any{
		"main": "version: 1\nstack: stack\ninclude:\n  - extra.yml\nsecrets:\n  MINT_ME:\n    default: generated\nenvironments:\n  prod:\n",
		"files": map[string]string{
			"extra.yml": "environments:\n  prod:\n    tiles:\n      api:\n        type: service\n        image: nginx\n        port: 8080\n",
		},
	})
	rec, err := call(t, a, a.previewPlan, http.MethodPost, "/", string(body), seed.Stack.ID, ScopeConfigRead)
	require.NoError(t, err, "preview")
	var got planDetailOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "decode %s", rec.Body.String())
	assert.Equal(t, "preview", got.Status, "response = %+v", got)
	assert.Empty(t, got.ID, "a preview has no id to approve: %+v", got)
	found := false
	for _, ch := range got.Changes {
		if ch.Tile == "api" && ch.Kind == "create" {
			found = true
		}
	}
	assert.True(t, found, "changes = %+v, want a create for tile api (from the included file)", got.Changes)
	// A to-be-minted secret must cross the wire, a CI gate that only counts
	// changes would call "will mint a secret" empty without it.
	assert.Contains(t, got.GenSecrets, "MINT_ME", "gen_secrets = %v", got.GenSecrets)
	plans, perr := s.ListConfigPlans(context.Background(), seed.Stack.ID, 10)
	require.NoError(t, perr, "list plans")
	assert.Empty(t, plans, "preview stored a plan row: %+v", plans)
}

// The read gate skips the binding check for history's sake; preview must not
// inherit that, an unbound stack has no branch context to diff against.
func TestPreviewPlanRejectsUnmanagedStack(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	a := apiFor(s)

	_, err := call(t, a, a.previewPlan, http.MethodPost, "/", `{"main":"version: 1"}`, seed.Stack.ID, ScopeConfigRead)
	wantStatus(t, err, http.StatusConflict, "previewing an unmanaged stack")
}

// The org preview twin: computes from posted bytes, stores no org plan row,
// and rejects an unbound org the same way the stack route does.
func TestOrgPreviewPlanComputesWithoutStoring(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	ctx := context.Background()
	seed.Org.ConfigConnectorID, seed.Org.ConfigRepo = "conn1", "org/cfg"
	require.NoError(t, s.UpdateOrg(ctx, seed.Org), "bind org")
	a := apiFor(s)

	// Declares a new org-scoped instance next to nothing → one create change.
	body := `{"main":"version: 1\norg: org\nshared:\n  pg:\n    type: managed\n    engine: postgres\n"}`
	rec, err := call(t, a, a.previewOrgPlan, http.MethodPost, "/", body, seed.Org.ID, ScopeConfigRead)
	require.NoError(t, err, "org preview")
	var got planDetailOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "decode %s", rec.Body.String())
	assert.Equal(t, "preview", got.Status, "response = %+v", got)
	found := false
	for _, ch := range got.Changes {
		if ch.Tile == "pg" && ch.Kind == "create" {
			found = true
		}
	}
	assert.True(t, found, "changes = %+v, want a create for shared pg", got.Changes)
	plans, perr := s.ListOrgConfigPlans(ctx, seed.Org.ID, 10)
	require.NoError(t, perr, "list org plans")
	assert.Empty(t, plans, "org preview stored a plan row: %+v", plans)

	// Unbound org → 409.
	seed.Org.ConfigConnectorID, seed.Org.ConfigRepo = "", ""
	require.NoError(t, s.UpdateOrg(ctx, seed.Org), "unbind org")
	_, err = call(t, a, a.previewOrgPlan, http.MethodPost, "/", body, seed.Org.ID, ScopeConfigRead)
	wantStatus(t, err, http.StatusConflict, "previewing an unbound org")
}

// A CI gate reads a listing, and a listing that omitted the destructive flag
// decoded to false on a plan that deletes things, the worst possible default.
func TestPlanListCarriesDestructive(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	seedPlan(t, s, seed.Stack.ID, "pending") // planBlob contains a delete
	a := apiFor(s)

	rec, err := call(t, a, a.listPlans, http.MethodGet, "/", "", seed.Stack.ID, ScopeConfigRead)
	require.NoError(t, err, "list")
	var got []planOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "decode")
	require.Len(t, got, 1, "listing = %+v, want the destructive flag set", got)
	require.True(t, got[0].Destructive, "listing = %+v, want the destructive flag set", got)
}

// Preview stores nothing, but it still runs the planner against live state,
// which is a write-role action like plan. It used to take the read path and so
// ran for any org viewer holding config:read.
func TestPreviewRequiresOrgWrite(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	a := apiFor(s)
	orgs := []string{seed.Org.ID}

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, s.CreateUser(ctx, &repo.User{ID: "outsider", Email: "v@example.com",
		Name: "Viewer", Role: "user", Active: true, CreatedAt: now, UpdatedAt: now}), "seed user")
	require.NoError(t, s.UpsertOrgMember(ctx, &repo.OrgMember{
		OrgID: seed.Org.ID, UserID: "outsider", Role: "viewer", CreatedAt: now}), "seed viewer")

	// Through the route's gate, not the bare handler: point 18 moved the
	// check off the body and onto the route, so calling the handler direct
	// would now assert nothing.
	_, err := callAs(t, a, a.gate(service.VerbStackPlan, service.KindStack, "id", a.previewPlan),
		http.MethodPost, "/", `{"main":"version: 1"}`, seed.Stack.ID, orgs, ScopeConfigRead)
	wantStatus(t, err, http.StatusForbidden, "viewer previewing a stack plan")

	_, err = callAs(t, a, a.gate(service.VerbOrgConfigBind, service.KindOrg, "id", a.previewOrgPlan),
		http.MethodPost, "/", `{"main":"version: 1"}`, seed.Org.ID, orgs, ScopeConfigRead)
	wantStatus(t, err, http.StatusForbidden, "viewer previewing an org plan")
}
