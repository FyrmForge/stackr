package web_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// The wizard branches on step 1 and the branch decides which steps exist. A
// config-managed org is named and given its domains by its file, so being asked
// for either is the bug this replaced: the name typed on step 1 used to be
// thrown away by the apply seconds later.
func TestJourneyConfigBranchHasNoNameOrDomainStep(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")
	slug := j.createDraft("config")

	o := mustOrg(t, j, slug)
	require.Equal(t, slug, o.Slug)

	for _, step := range []string{"name", "domain"} {
		code, loc := j.get("/orgs/" + slug + "/setup/" + step)
		require.Equal(t, http.StatusSeeOther, code, "step %s is not on this branch", step)
		require.Equal(t, "/orgs/"+slug+"/setup/done", loc, "step %s", step)
	}

	summary := j.page("/orgs/" + slug + "/setup/done")
	require.Contains(t, summary, "Config as code")
	require.NotContains(t, summary, ">Domain<",
		"the file declares domains and Finish writes a default, so the row would read not done while about to be true")

	// The UI branch is the mirror image: it has both, and no config step.
	ui := j.createOrg("Acme")
	require.Equal(t, "acme", ui, "the name step is what names a hand-built org")
	code, loc := j.get("/orgs/" + ui + "/setup/config")
	require.Equal(t, http.StatusSeeOther, code, "the config step is not on the ui branch")
	require.Equal(t, "/orgs/"+ui+"/setup/done", loc)
	require.Contains(t, j.page("/orgs/"+ui+"/setup/done"), ">Domain<")
}

// One draft per person. Going back to /setup and picking the other branch is
// the same organization changing its mind, not a second one, and matching on
// the placeholder name would miss a draft the ui branch already renamed.
func TestJourneyDraftIsReused(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")

	first := j.createDraft("config")
	again := j.createDraft("ui")
	require.Equal(t, first, again, "the second answer moves the draft, it does not make another")

	named := j.createOrg("Acme")
	require.Equal(t, "acme", named)

	// The renamed draft is still the draft, so /setup does not start a third.
	reused := j.createDraft("config")
	require.Equal(t, "acme", reused, "an unfinished org is a draft whatever it is called")

	orgs, err := j.store.ListOrgs(context.Background())
	require.NoError(t, err)
	require.Len(t, orgs, 1, "one draft, three answers")
}

// Switching to the ui branch has to clear the binding. Leaving it behind keeps
// ConfigManaged() true with no file behind it, which is the state where the org
// is named after a placeholder and no config is ever reconciled.
func TestJourneySwitchBranchClearsBinding(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")
	slug := j.createDraft("config")
	j.bindConfig(slug, "version: 1\norg: Initech\nvars:\n  REGION: eu\n")

	plan := j.page("/orgs/" + slug + "/setup/config/plan")
	require.Contains(t, plan, "Review the plan")

	page := j.page("/orgs/" + slug + "/setup/config")
	code, loc := j.post(page, "/orgs/"+slug+"/setup/mode", url.Values{"mode": {"ui"}})
	require.Equal(t, "/orgs/"+slug+"/setup/name", loc, "switching lands on the branch's first step, code %d", code)

	ctx := context.Background()
	o, err := j.store.GetOrgBySlug(ctx, slug)
	require.NoError(t, err)
	require.False(t, o.ConfigManaged(), "the binding is gone")
	require.Empty(t, o.ConfigRepo)
	require.Empty(t, o.ConfigBranch)
	require.Empty(t, o.ConfigPath)

	plans, err := j.store.ListOrgConfigPlans(ctx, o.ID, 20)
	require.NoError(t, err)
	require.Len(t, plans, 1)
	require.Equal(t, "rejected", plans[0].Status, "a plan waiting on a binding nobody kept must not survive it")

	require.Contains(t, j.page("/orgs/"+slug+"/setup/name"), "Name your organization")
}

// Discard is the way out of a draft. On a fresh install it is the only org
// there is, so the "last organization" refusal would leave the wizard with no
// exit at all, and settings are shut so its refusals cannot land there either.
func TestJourneyDiscardDraft(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")
	slug := j.createOrg("Acme")

	summary := j.page("/orgs/" + slug + "/setup/done")
	require.Contains(t, summary, "/orgs/"+slug+"/delete", "the summary is where Discard lives")

	// Discard sends the hx-confirm dialog and nothing else, and a draft has
	// nothing behind it to protect: asking for a typed slug on top made every
	// Discard a 400 with the draft still there and nothing on screen.
	code, loc := j.post(summary, "/orgs/"+slug+"/delete", url.Values{})
	require.Equal(t, "/", loc, "discarding goes back to the canvas, code %d", code)

	o, err := j.store.GetOrgBySlug(context.Background(), slug)
	require.NoError(t, err)
	require.Nil(t, o, "the draft is gone")

	// And /setup still works with no orgs left at all.
	j.page("/setup")
}

// The typed-name guard is still the guard everywhere it protects something: an
// org past Finish holds stacks, people and secrets, and deletes from settings.
func TestJourneyDeleteFinishedOrgNeedsTypedName(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")
	slug := j.createOrg("Acme")
	j.finishSetup(slug)

	page := j.page("/orgs/" + slug + "/settings/general")
	code, _ := j.post(page, "/orgs/"+slug+"/delete", url.Values{})
	require.Equal(t, 400, code, "no confirm field, no delete")

	code, _ = j.post(page, "/orgs/"+slug+"/delete", url.Values{"confirm": {"not-it"}})
	require.Equal(t, 400, code, "the typed name has to match")
}

// The bug this wizard was rebuilt for, from the other end: an org nobody has
// named must not be able to finish. On the config branch only the apply names
// it, so Finish before that would strand a real organization called
// "Untitled organization" at a random slug.
func TestJourneyUnnamedOrgCannotFinish(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")
	slug := j.createDraft("config")

	summary := j.page("/orgs/" + slug + "/setup/done")
	require.NotContains(t, summary, `hx-post="/orgs/`+slug+`/setup/done"`,
		"Finish is not offered while the org has no name")

	// And the endpoint refuses it too, not just the page that hides the button.
	step := j.page("/orgs/" + slug + "/setup/connector")
	code, loc := j.post(step, "/orgs/"+slug+"/setup/done", url.Values{})
	require.Equal(t, "/orgs/"+slug+"/setup/connector", loc, "back to the branch's first step, code %d", code)

	o, err := j.store.GetOrgBySlug(context.Background(), slug)
	require.NoError(t, err)
	require.Nil(t, o.SetupDoneAt, "the wizard is still open")
}

// Somebody whose only org is the one they are halfway through setting up is
// sent to the wizard, not to a canvas holding one tile they have to click.
func TestJourneyHomeResumesTheDraft(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")
	slug := j.createDraft("ui")

	code, loc := j.get("/")
	require.Equal(t, http.StatusSeeOther, code, "home with nothing but a draft: %s", loc)
	require.Equal(t, "/orgs/"+slug+"/setup/done", loc)

	// Once it is a real org, home is the org list again.
	named := j.createOrg("Acme")
	j.finishSetup(named)
	code, body := j.get("/")
	require.Equal(t, http.StatusOK, code, "home after finishing: %s", first(body, 200))
}

// A refused name comes back as the step with the name still in the box: the
// step is the only place to read the reason, and retyping a long name because
// of a typo is the sort of thing that makes people give up on a wizard.
func TestJourneyRefusedNameKeepsWhatWasTyped(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")
	taken := j.createOrg("Harbour Works")
	j.finishSetup(taken)

	slug := j.createDraft("ui")
	form := j.page("/orgs/" + slug + "/setup/name")
	code, body := j.post(form, "/orgs/"+slug+"/setup/name", url.Values{"name": {"Harbour Works"}})
	require.Equal(t, http.StatusOK, code, "a refusal renders the step, it does not redirect or 400")
	require.Contains(t, body, "Another organization already uses that name.")
	require.Contains(t, body, `value="Harbour Works"`, "the name stays in the box")

	// Punctuation alone has no slug in it, and that reason has to be readable.
	code, body = j.post(form, "/orgs/"+slug+"/setup/name", url.Values{"name": {"!!!"}})
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, body, "at least one letter or digit")

	o, err := j.store.GetOrgBySlug(context.Background(), slug)
	require.NoError(t, err)
	require.Equal(t, "Untitled organization", o.Name, "neither refusal renamed anything")
}
