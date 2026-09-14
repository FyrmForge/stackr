package web_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// The whole point of the wizard: a fresh install lands on it, every step past
// the first can be skipped, and it ends when the owner says it does.
func TestJourneyFirstBoot(t *testing.T) {
	j := newJourney(t)

	// A server with no accounts sends login to registration; there is nobody
	// to log in as yet.
	code, loc := j.get("/login")
	require.Equal(t, http.StatusSeeOther, code, "login on an empty server: %s", loc)
	require.Equal(t, "/register", loc)

	j.register("owner@example.com", "Correct-Horse9")
	slug := j.createOrg("Acme Corp")

	// Every step renders and every step can be skipped.
	for _, step := range []string{"name", "connector", "domain", "team", "done"} {
		j.page("/orgs/" + slug + "/setup/" + step)
	}

	// Step 4 is the one wizard write with no GitHub behind it: the domain
	// lands, and the form comes back to its own step rather than to settings.
	domain := j.page("/orgs/" + slug + "/setup/domain")
	code, loc = j.post(domain, "/orgs/"+slug+"/setup/domain", url.Values{"host": {"acme.example.com"}})
	require.Equal(t, "/orgs/"+slug+"/setup/team", loc, "adding the domain is the step; it must move on, code %d", code)

	o, err := j.store.GetOrgBySlug(context.Background(), slug)
	require.NoError(t, err)
	require.Nil(t, o.SetupDoneAt, "walking the steps must not finish the wizard")

	summary := j.page("/orgs/" + slug + "/setup/done")
	code, loc = j.post(summary, "/orgs/"+slug+"/setup/done", url.Values{})
	require.Equal(t, "/orgs/"+slug, loc, "Finish goes to the org, code %d", code)

	o, err = j.store.GetOrgBySlug(context.Background(), slug)
	require.NoError(t, err)
	require.NotNil(t, o.SetupDoneAt, "Finish is what closes the wizard")

	j.page("/orgs/" + slug + "/settings/members")

	// And the wizard is over: its URLs are stale copies of settings now.
	code, loc = j.get("/orgs/" + slug + "/setup/team")
	require.Equal(t, http.StatusSeeOther, code)
	require.Equal(t, "/orgs/"+slug+"/settings/members", loc)
}

// Settings stay shut until the wizard is finished, or the flow is a suggestion:
// every step links to a tab, and one click used to eject the owner with the
// steps behind them undone and nothing saying so.
func TestJourneySettingsLocked(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")
	slug := j.createOrg("Acme")

	for _, tab := range []string{"general", "members", "connectors", "variables", "domains", "config"} {
		code, loc := j.get("/orgs/" + slug + "/settings/" + tab)
		require.Equal(t, http.StatusSeeOther, code, "settings/%s stayed open mid-setup", tab)
		require.Equal(t, "/orgs/"+slug+"/setup/done", loc, "settings/%s", tab)
	}

	// Nor is the canvas: an unfinished org answers nothing but its own wizard,
	// or the steps are just a suggestion you click around (plan 29).
	code, loc := j.get("/orgs/" + slug)
	require.Equal(t, http.StatusSeeOther, code, "the canvas stayed open mid-setup")
	require.Equal(t, "/orgs/"+slug+"/setup/done", loc, "the canvas")

	summary := j.page("/orgs/" + slug + "/setup/done")
	_, _ = j.post(summary, "/orgs/"+slug+"/setup/done", url.Values{})
	for _, tab := range []string{"general", "members", "connectors"} {
		j.page("/orgs/" + slug + "/settings/" + tab)
	}
}

// Step 5 and the People tab are one panel over one set of endpoints. The wizard
// copy has to stay in the wizard: adding somebody used to redirect into
// settings, which ended the flow halfway through.
func TestJourneyTeam(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")
	slug := j.createOrg("Acme")

	step := j.page("/orgs/" + slug + "/setup/team")
	code, loc := j.post(step, "/orgs/"+slug+"/setup/team/members", url.Values{
		"email": {"new@example.com"}, "role": {"member"}, "expires_days": {"14"},
	})
	require.Equal(t, "/orgs/"+slug+"/setup/team", loc, "adding somebody must not leave the wizard, code %d", code)

	step = j.page("/orgs/" + slug + "/setup/team")
	require.Contains(t, step, "new@example.com", "the invite shows in the step that made it")
	require.Contains(t, step, "/orgs/"+slug+"/setup/team/invites/", "its buttons stay on the wizard's routes")

	invites, err := j.store.ListInvitesByOrg(context.Background(), (mustOrg(t, j, slug)).ID)
	require.NoError(t, err)
	require.Len(t, invites, 1)

	code, loc = j.post(step, "/orgs/"+slug+"/setup/team/invites/"+invites[0].ID+"/delete", url.Values{})
	require.Equal(t, "/orgs/"+slug+"/setup/team", loc, "revoking stays too, code %d", code)
	invites, err = j.store.ListInvitesByOrg(context.Background(), (mustOrg(t, j, slug)).ID)
	require.NoError(t, err)
	require.Empty(t, invites)

	// The owner's own row offers nothing that could lock them out.
	step = j.page("/orgs/" + slug + "/setup/team")
	require.NotContains(t, step, "/remove", "own row must not offer Remove")
}

func mustOrg(t *testing.T, j *journey, slug string) *orgRow {
	t.Helper()
	o, err := j.store.GetOrgBySlug(context.Background(), slug)
	require.NoError(t, err)
	require.NotNil(t, o)
	return &orgRow{ID: o.ID, Slug: o.Slug}
}

type orgRow struct{ ID, Slug string }
