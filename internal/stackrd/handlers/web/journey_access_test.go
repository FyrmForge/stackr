package web_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The point-15 wizard raise. /setup/domain and /setup/connector are served by
// the handlers that serve org settings, where member is the right level, and
// the setup allow-list waives the draft gate for the whole /setup prefix — so
// before this a member who could not see the wizard could still post its
// domain step. Every other step of the wizard is owner.
func TestJourneyAMemberCannotPostAWizardStep(t *testing.T) {
	j := newJourney(t)
	j.register("owner@example.com", "Correct-Horse9")
	slug := j.createOrg("Acme Corp")

	// The owner reaches it, which is the control: a refusal below has to come
	// from the role and not from the route being broken.
	domain := j.page("/orgs/" + slug + "/setup/domain")
	code, loc := j.post(domain, "/orgs/"+slug+"/setup/domain", url.Values{"host": {"acme.example.com"}})
	require.Equal(t, "/orgs/"+slug+"/setup/team", loc, "the owner posts the step, code %d", code)

	// Seat a second account as a member of the same org and sign in as them.
	ctx := context.Background()
	o, err := j.store.GetOrgBySlug(ctx, slug)
	require.NoError(t, err)
	// /register is first-boot only, so the second account is minted through
	// AuthService and then signed in over HTTP like any other user.
	u, err := service.NewAuthService(j.store).Register(ctx, "member@example.com", "Correct-Horse9", "Member")
	require.NoError(t, err)
	require.NoError(t, j.store.UpsertOrgMember(ctx, &repo.OrgMember{
		OrgID: o.ID, UserID: u.ID, Role: "member", CreatedAt: time.Now().UTC()}))
	j.post(domain, "/logout", url.Values{})
	login := j.page("/login")
	code, loc = j.post(login, "/login", url.Values{
		"email": {"member@example.com"}, "password": {"Correct-Horse9"}})
	require.Contains(t, []int{http.StatusOK, http.StatusSeeOther, http.StatusFound}, code, "member login: %s", first(loc, 300))

	// The member cannot even open the wizard, which is the half that always
	// worked. The POST is the half that did not: the setup allow-list waives
	// the draft gate for the whole prefix, so it arrived at a handler that
	// only asked for member. Its CSRF token comes off a page they can load.
	code, _ = j.get("/orgs/" + slug + "/setup/domain")
	require.Equal(t, http.StatusNotFound, code, "a member does not see the wizard")

	own := j.page("/account/profile")
	code, body := j.post(own, "/orgs/"+slug+"/setup/domain", url.Values{"host": {"member.example.com"}})
	require.Equal(t, http.StatusForbidden, code, "a member must not post a wizard step: %s", first(body, 300))

	code, body = j.post(own, "/orgs/"+slug+"/setup/connector", url.Values{})
	require.Equal(t, http.StatusForbidden, code, "nor the connector step: %s", first(body, 300))
}
