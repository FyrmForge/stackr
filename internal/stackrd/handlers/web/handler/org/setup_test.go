package org

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// The wizard and the settings tab share one handler, and the route the request
// arrived on is the only thing that says which. A form field used to say it,
// which made every one of those endpoints an open-redirect candidate; a
// registered path cannot be pointed anywhere this server does not serve.
func TestBackToFollowsTheRoute(t *testing.T) {
	o := &repo.Org{Slug: "acme"}
	const def = "/orgs/acme/settings/members"
	for path, want := range map[string]string{
		"/orgs/:slug/setup/team/members":                "/orgs/acme/setup/team",
		"/orgs/:slug/setup/team/members/:userID/remove": "/orgs/acme/setup/team",
		"/orgs/:slug/setup/domain":                      "/orgs/acme/setup/domain",
		"/orgs/:slug/members":                           def,
		"/orgs/:slug/settings/domains":                  def,
	} {
		c := echo.New().NewContext(httptest.NewRequest("POST", "/", nil), httptest.NewRecorder())
		c.SetPath(path)
		require.Equal(t, want, backTo(c, o, def), "path=%q", path)
	}
}

// Adding a person always creates an invite bound to the folded address, and
// never a membership: joining an account to an org it never agreed to is not
// the owner's call, and branching on whether the address has an account made
// this an account-existence oracle for anyone who owns an org.
func TestAddMemberAlwaysInvites(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	now := time.Now().UTC()
	owner, orgs, _, _, _ := ownerHereViewerThere(t, s)
	org := orgs[0]
	joiner := &repo.User{ID: "u2", Email: "joiner@example.com", Name: "Joiner", Role: "user", Active: true, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateUser(ctx, joiner))
	h := NewHandler(s, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).
		WithDomainResources(service.NewDomainResourceService(s, nil)).
		WithPlans(service.NewPlanService(s, nil, nil)).
		WithGraph(service.NewGraphService(s))

	add := func(email string) error {
		c := asUser(t, http.MethodPost, "email="+url.QueryEscape(email)+"&role=viewer", owner, orgs, "owner")
		c.SetParamNames("id")
		c.SetParamValues(org.ID)
		return h.AddMember(c)
	}

	require.NoError(t, add("nobody@example.com"))
	require.NoError(t, add("  Joiner@Example.com "))

	m, _ := s.GetOrgMember(ctx, org.ID, joiner.ID)
	require.Nil(t, m, "an existing account is invited, not joined")

	invs, err := s.ListInvitesByOrg(ctx, org.ID)
	require.NoError(t, err)
	var addrs []string
	for _, in := range invs {
		addrs = append(addrs, in.Email)
	}
	require.ElementsMatch(t, []string{"nobody@example.com", "joiner@example.com"}, addrs,
		"both addresses get an invite, folded, so neither answer reveals whether the account exists")
}

func TestSetupDomainPrefill(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	h := NewHandler(s, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).
		WithDomainResources(service.NewDomainResourceService(s, nil)).
		WithPlans(service.NewPlanService(s, nil, nil)).
		WithGraph(service.NewGraphService(s))
	o := &repo.Org{Slug: "acme"}
	components.BaseURL = ""
	require.Equal(t, "", h.setupDomainPrefill(ctx, o))
	components.BaseURL = "https://panel.example.com"
	require.Equal(t, "acme.panel.example.com", h.setupDomainPrefill(ctx, o))
	// The installer's root domain wins over the panel's own host.
	require.NoError(t, s.CreateDomainResource(ctx, &repo.DomainResource{ID: "r1", Level: "instance", OwnerID: "local", Host: "example.com", CreatedAt: time.Now()}))
	require.Equal(t, "acme.example.com", h.setupDomainPrefill(ctx, o))
	components.BaseURL = ""
}

// Anti-squat is only real if every path that accepts a hostname enforces it:
// org resources, tile domains and config as code all route through this.
func TestCheckOrgSquat(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	other := &repo.Org{ID: "orgX", Name: "Other", Slug: "other", CreatedAt: time.Now().UTC()}
	require.NoError(t, s.CreateOrg(ctx, other))

	require.Error(t, service.CheckOrgSquat(ctx, s, "other.example.com", "mine"), "another org's slug as first label")
	require.Error(t, service.CheckOrgSquat(ctx, s, "*.other.example.com", "mine"), "wildcard hides the same claim")
	require.NoError(t, service.CheckOrgSquat(ctx, s, "other.example.com", other.ID), "the org's own slug is fine")
	require.NoError(t, service.CheckOrgSquat(ctx, s, "unrelated.example.com", "mine"))
	require.NoError(t, service.CheckOrgSquat(ctx, s, "example.com", "mine"), "single-label hosts have no org prefix")
}

// The People list is one list built from two tables, so it is the only place
// that decides what a row IS. Members first (they are the answer to "who is in
// this org"), invites after, and an invite past its expiry has to read as
// expired rather than as something a Resend button would still work on.
func TestPeopleRowsOrderAndState(t *testing.T) {
	now := time.Now().UTC()
	rows := peopleRows(
		[]repo.OrgMember{{UserID: "u1", Name: "Owner", Email: "o@example.com", Role: "owner"}},
		[]repo.Invite{
			{ID: "tok1", Email: "fresh@example.com", Role: "member", ExpiresAt: now.AddDate(0, 0, 14)},
			{ID: "tok2", Email: "stale@example.com", Role: "viewer", ExpiresAt: now.AddDate(0, 0, -1)},
		},
	)
	require.Len(t, rows, 3)
	require.Equal(t, []string{"member", "pending", "expired"},
		[]string{rows[0].State, rows[1].State, rows[2].State})
	require.Equal(t, "u1", rows[0].UserID)
	require.Nil(t, rows[0].Invite, "a member has no invite to act on")
	require.Equal(t, "tok1", rows[1].Invite.ID, "each invite row must carry its own token")
	require.Equal(t, "tok2", rows[2].Invite.ID)
	require.Equal(t, "stale@example.com", rows[2].Name, "an invite has no name but its address")
}

// Reinvite exists because the token IS the row id: an expired invite cannot be
// extended, it has to be replaced. The new row must carry the same address and
// role under a different token, and the dead one must be gone.
func TestReinviteMintsANewToken(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	owner, orgs, _, _, _ := ownerHereViewerThere(t, s)
	org := orgs[0]
	dead := &repo.Invite{
		ID: "deadtoken", OrgID: org.ID, Email: "late@example.com", Role: "viewer",
		CreatedAt: time.Now().UTC().AddDate(0, 0, -30), ExpiresAt: time.Now().UTC().AddDate(0, 0, -16),
	}
	require.NoError(t, s.CreateInvite(ctx, dead))
	h := NewHandler(s, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).
		WithDomainResources(service.NewDomainResourceService(s, nil)).
		WithPlans(service.NewPlanService(s, nil, nil)).
		WithGraph(service.NewGraphService(s))

	c := asUser(t, http.MethodPost, "", owner, orgs, "owner")
	c.SetParamNames("id", "inviteID")
	c.SetParamValues(org.ID, dead.ID)
	require.NoError(t, h.ReinviteMember(c))

	gone, err := s.GetInvite(ctx, dead.ID)
	require.NoError(t, err)
	require.Nil(t, gone, "the expired invite must not stay usable")

	live, err := s.ListInvitesByOrg(ctx, org.ID)
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.NotEqual(t, dead.ID, live[0].ID, "a fresh token, not the old one")
	require.Equal(t, "late@example.com", live[0].Email)
	require.Equal(t, "viewer", live[0].Role)
	require.True(t, live[0].ExpiresAt.After(time.Now().UTC()))
}

// Rendering the summary used to close the wizard, so glancing at step 6, or
// hitting back into it, flipped the flag and dropped the owner into settings
// on the next click, with steps they never did marked as behind them.
func TestSetupDoneOnlyOnPost(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	u, orgs, _, _, _ := ownerHereViewerThere(t, s)
	org := orgs[0]
	// The seed is an onboarded org; this test is about the wizard, so put it
	// back mid-flow.
	org.SetupDoneAt = nil
	require.NoError(t, s.UpdateOrg(ctx, &org))
	orgs[0] = org
	h := NewHandler(s, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).
		WithDomainResources(service.NewDomainResourceService(s, nil)).
		WithPlans(service.NewPlanService(s, nil, nil)).
		WithGraph(service.NewGraphService(s))

	get := asUser(t, http.MethodGet, "", u, orgs, "owner")
	get.Set("csrf", "test-token") // the summary carries the Finish form
	// The route pattern, not just the params: the setup gate reads it to know
	// this request is the wizard itself and not a sideways step into the org.
	get.SetPath("/orgs/:slug/setup/:step")
	get.SetParamNames("slug", "step")
	get.SetParamValues(org.Slug, "done")
	require.NoError(t, h.Setup(get))
	after, err := s.GetOrg(ctx, org.ID)
	require.NoError(t, err)
	require.Nil(t, after.SetupDoneAt, "looking at the summary must not finish the wizard")

	post := asUser(t, http.MethodPost, "", u, orgs, "owner")
	post.SetPath("/orgs/:slug/setup/done")
	post.SetParamNames("slug")
	post.SetParamValues(org.Slug)
	require.NoError(t, h.SetupDone(post))
	after, err = s.GetOrg(ctx, org.ID)
	require.NoError(t, err)
	require.NotNil(t, after.SetupDoneAt, "Finish is what closes it")
}

// An org that finished setup with no domain anywhere used to get no hostnames
// at all: EnsureAutoDomain returns early with nothing to nest under, so every
// tile came up unreachable and nothing said why. Both branches land here, a
// config file with no domains: block, and "Skip for now" past the form that
// had this exact host prefilled.
func TestSetupDoneCreatesDefaultDomain(t *testing.T) {
	ctx := context.Background()
	components.BaseURL = "https://panel.example.com"
	t.Cleanup(func() { components.BaseURL = "" })

	finish := func(t *testing.T, seed func(*testing.T, *sqlite.Store, *repo.Org)) []repo.DomainResource {
		t.Helper()
		s := testdb.New(t)
		u, orgs, _, _, _ := ownerHereViewerThere(t, s)
		org := orgs[0]
		org.SetupDoneAt = nil
		require.NoError(t, s.UpdateOrg(ctx, &org))
		orgs[0] = org
		if seed != nil {
			seed(t, s, &org)
		}
		h := NewHandler(s, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).
			WithDomainResources(service.NewDomainResourceService(s, nil)).
			WithPlans(service.NewPlanService(s, nil, nil)).
			WithGraph(service.NewGraphService(s))
		c := asUser(t, http.MethodPost, "", u, orgs, "owner")
		c.SetPath("/orgs/:slug/setup/done")
		c.SetParamNames("slug")
		c.SetParamValues(org.Slug)
		require.NoError(t, h.SetupDone(c))
		res, err := s.ListDomainResources(ctx)
		require.NoError(t, err)
		return res
	}

	res := finish(t, nil)
	require.Len(t, res, 1, "an org with nothing to nest under gets a default")
	require.Equal(t, "org.panel.example.com", res[0].Host, "the org slug under the server's own host")
	require.Equal(t, "org", res[0].Level)
	require.False(t, res[0].Declared,
		"undeclared, so orgconf.applyDomains never deletes it and a declared domain outranks it")

	// Anything already visible is enough, including a server-wide row every
	// org inherits, which is not the org's to duplicate.
	for _, tc := range []struct {
		name string
		res  *repo.DomainResource
	}{
		{"its own", &repo.DomainResource{ID: "x", Level: "org", OwnerID: "org1", Host: "mine.example"}},
		{"the server's", &repo.DomainResource{ID: "y", Level: "instance", OwnerID: "local", Host: "server.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := finish(t, func(t *testing.T, s *sqlite.Store, _ *repo.Org) {
				require.NoError(t, s.CreateDomainResource(ctx, tc.res))
			})
			require.Len(t, got, 1, "no default on top of a domain the org can already use")
			require.Equal(t, tc.res.Host, got[0].Host)
		})
	}

	// A LAN install has no base domain to build one from.
	components.BaseURL = ""
	require.Empty(t, finish(t, nil), "no BASE_URL, no default")
}
