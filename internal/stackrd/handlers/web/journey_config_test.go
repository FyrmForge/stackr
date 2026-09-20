package web_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// bindConfig seeds a connector and points the org's config at a file the fake
// source serves. The GitHub App round trip that creates the connector for real
// is the one thing these tests cannot stand in for, so it stays manual QA.
func (j *journey) bindConfig(slug, body string) {
	j.t.Helper()
	ctx := context.Background()
	o, err := j.store.GetOrgBySlug(ctx, slug)
	require.NoError(j.t, err)
	require.NotNil(j.t, o)
	cn := &repo.Connector{ID: "cn1", OrgID: o.ID, Provider: "github", Name: "acme-app",
		CreatedAt: time.Now().UTC()}
	require.NoError(j.t, j.store.CreateConnector(ctx, cn))
	j.src.files["stackr-org.yml"] = body

	// Step 3 renders its install gate rather than the binding form without a
	// GitHub client to list repositories with, so the token comes off step 5.
	// The binding endpoint is the same one the form would post to.
	page := j.page("/orgs/" + slug + "/setup/team")
	code, loc := j.post(page, "/orgs/"+slug+"/setup/config", url.Values{
		"connector_id": {cn.ID}, "repo": {"acme/infra"},
		"branch": {"main"}, "path": {"stackr-org.yml"},
	})
	require.Equal(j.t, "/orgs/"+slug+"/setup/config/plan", loc,
		"binding a file lands on the plan it just produced, code %d", code)
}

// awaitApply is the wizard's plan screen doing what a browser does with it:
// polling while the queued apply runs, and following the redirect the screen
// answers with once the job has landed. Returns where it sent us.
func (j *journey) awaitApply(planURL string) string {
	j.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code, loc := j.get(planURL)
		if code == http.StatusSeeOther {
			return loc
		}
		time.Sleep(10 * time.Millisecond)
	}
	j.t.Fatalf("the apply never finished: %s still rendering", planURL)
	return ""
}

// Binding a file in the wizard has to show the plan it produced. Sending the
// owner on to step 4 without it means the org they just described never gets
// built and nothing on screen says so.
func TestJourneyConfigPlanInWizard(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")
	slug := j.createDraft("config")

	j.bindConfig(slug, "version: 1\norg: Acme\nvars:\n  REGION: eu\nsecrets:\n  STRIPE_KEY:\n")
	plan := j.page("/orgs/" + slug + "/setup/config/plan")
	require.Contains(t, plan, "Review the plan")
	require.Contains(t, plan, "REGION", "the plan says what it will set")
	require.NotContains(t, plan, "Decide later",
		"this branch's org is named and built by the apply: there is no walking past it")

	// The plan stays pending until it is decided. The canvas is gated until the
	// wizard is finished (plan 29), so the wizard's own plan screen is the way
	// back to it.
	code, loc := j.get("/orgs/" + slug)
	require.Equal(t, http.StatusSeeOther, code, "the canvas is open mid-setup")
	require.Equal(t, "/orgs/"+slug+"/setup/done", loc)
	require.Contains(t, j.page("/orgs/"+slug+"/setup/config/plan"), "Review the plan",
		"the pending plan is still reachable from inside the wizard")

	code, loc = j.post(plan, planAction(t, plan, "approve"), url.Values{})
	// Approving queues the apply and stays on the plan screen, which then
	// polls itself. The URL is the org's id, not its slug: the apply is about
	// to rename the org, and the slug this request arrived on will be gone by
	// the time the next poll goes out.
	o, err := j.store.GetOrgBySlug(context.Background(), slug)
	require.NoError(t, err)
	require.Equal(t, "/orgs/"+o.ID+"/setup/config/plan?plan="+planIDFrom(t, plan), loc,
		"approving stays on the plan while it applies, code %d", code)

	// The apply is what names a config-managed org, so the step it advances to
	// is under the slug the file gave it, not the placeholder the draft had.
	require.Equal(t, "/orgs/acme/setup/team", j.awaitApply(loc),
		"the plan screen moves the wizard on once the apply lands")

	o, err = j.store.GetOrgBySlug(context.Background(), "acme")
	require.NoError(t, err)
	vars, err := j.store.ListVariables(context.Background(), repo.OwnerOrg, o.ID)
	require.NoError(t, err)
	require.Len(t, vars, 1)
	require.Equal(t, "REGION", vars[0].Name, "the apply actually ran")
}

// A value the config declares and nobody has set is an input, not a blocker.
// It used to be a plan error, which refused the apply that would have created
// it, a stack could never be stood up from its own config.
func TestJourneyPlanInputs(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")
	slug := j.createDraft("config")

	j.bindConfig(slug, "version: 1\norg: Acme\nsecrets:\n  STRIPE_KEY:\n    required: true\n  SESSION_SECRET:\n    default: generated\n")
	plan := j.page("/orgs/" + slug + "/setup/config/plan")
	require.Contains(t, plan, "STRIPE_KEY", "a required secret nobody set is offered as a box")
	require.Contains(t, plan, `name="value"`)
	require.NotContains(t, plan, "disabled", "and it does not block the apply")

	code, loc := j.post(plan, planAction(t, plan, "inputs"), url.Values{
		"scope": {"org"}, "name": {"STRIPE_KEY"}, "value": {"sk_test_1"},
	})
	require.Equal(t, "/orgs/"+slug+"/setup/config/plan", loc, "Set re-plans in place, code %d", code)

	plan = j.page("/orgs/" + slug + "/setup/config/plan")
	require.NotContains(t, plan, `name="value"`, "the box is gone once the value exists")

	_, loc = j.post(plan, planAction(t, plan, "approve"), url.Values{})
	j.awaitApply(loc)
	o, err := j.store.GetOrgBySlug(context.Background(), "acme") // the file named it
	require.NoError(t, err)
	vars, err := j.store.ListVariables(context.Background(), repo.OwnerOrg, o.ID)
	require.NoError(t, err)
	byName := map[string]string{}
	for _, v := range vars {
		byName[v.Name] = v.Value
	}
	require.Equal(t, "sk_test_1", byName["STRIPE_KEY"], "the value set on the plan page is the one applied")
	require.NotEmpty(t, byName["SESSION_SECRET"], "a generated secret is minted by the apply")
}

// An org file may rename the org, which moves every URL under it. Redirecting
// to the slug the request arrived on then 404s on the org that was just built.
func TestJourneyRename(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")
	slug := j.createDraft("config")

	j.bindConfig(slug, "version: 1\norg: Initech\n")
	plan := j.page("/orgs/" + slug + "/setup/config/plan")
	_, loc := j.post(plan, planAction(t, plan, "approve"), url.Values{})
	require.Equal(t, "/orgs/initech/setup/team", j.awaitApply(loc),
		"the step the plan screen advances to uses the post-apply slug")

	code, _ := j.get("/orgs/" + slug + "/setup/team")
	require.Equal(t, http.StatusNotFound, code, "the old slug is gone")
	j.page("/orgs/initech/setup/team")
}

// planIDFrom reads the plan's id off its own approve button, so the test never
// has to guess at an id the page already carries.
func planIDFrom(t *testing.T, page string) string {
	t.Helper()
	action := planAction(t, page, "approve")
	parts := strings.Split(strings.TrimSuffix(action, "/approve"), "/")
	return parts[len(parts)-1]
}

// planAction finds the plan page's own button target, so the test posts where
// the page says rather than where it assumes.
func planAction(t *testing.T, page, verb string) string {
	t.Helper()
	for _, part := range strings.Split(page, `hx-post="`)[1:] {
		if action := part[:strings.IndexByte(part, '"')]; strings.HasSuffix(action, "/"+verb) {
			return action
		}
	}
	t.Fatalf("no %s button on the plan page", verb)
	return ""
}

// Stacks were called projects, and links to the old plan URL are already out
// there (banners, flashes, bookmarks) so it redirects rather than 404s.
func TestJourneyLegacyPlanURL(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")
	slug := j.createOrg("Acme")

	ctx := context.Background()
	o, err := j.store.GetOrgBySlug(ctx, slug)
	require.NoError(t, err)
	now := time.Now().UTC()
	stack := &repo.Stack{ID: "s1", OrgID: o.ID, Name: "Web", Slug: "web", CreatedAt: now}
	require.NoError(t, j.store.CreateStack(ctx, stack))
	cp := &repo.ConfigPlan{ID: "p1", StackID: stack.ID, Status: "pending",
		Summary: "1 change", Plan: `{"changes":[]}`, CreatedAt: now}
	require.NoError(t, j.store.CreateConfigPlan(ctx, cp))

	// Finish the wizard first: an unfinished org answers nothing but its own
	// wizard, and this is about the old URL, not about the gate.
	j.finishSetup(slug)

	code, loc := j.get("/projects/" + stack.ID + "/config/plans/" + cp.ID)
	require.Equal(t, http.StatusSeeOther, code, "old plan URL: %s", loc)
	require.Equal(t, "/"+slug+"/web/plans/"+cp.ID, loc)
	j.page(loc)
}
