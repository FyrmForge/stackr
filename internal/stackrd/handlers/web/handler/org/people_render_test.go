package org

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	hamrctx "github.com/FyrmForge/hamr/pkg/ctx"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The roster half of the add panel only renders for a server admin looking at
// an org that other accounts are missing from, which no single-user install can
// reach, so it gets rendered here instead of never.
func TestPeoplePanelRendersRoster(t *testing.T) {
	c := echo.New().NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
	c.Set("csrf", "test-token") // CSRFField reads it off the context and panics without it
	o := &repo.Org{ID: "o1", Name: "Acme", Slug: "acme"}
	candidates := []repo.User{{ID: "u9", Name: "Ada Lovelace", Email: "ada@example.com"}}

	var buf bytes.Buffer
	require.NoError(t, peoplePanel(c, o, "/orgs/acme", candidates, true).Render(context.Background(), &buf))
	out := buf.String()
	require.Contains(t, out, "Ada Lovelace", "roster row")
	require.Contains(t, out, "ada@example.com", "the add form posts the candidate's address")
	require.Contains(t, out, "/orgs/acme/members", "roster add posts to the member endpoint, addressed by slug")
	require.Contains(t, out, "emailed to this address", "mail-enabled copy")

	buf.Reset()
	require.NoError(t, peoplePanel(c, o, "/orgs/acme", nil, false).Render(context.Background(), &buf))
	out = buf.String()
	require.NotContains(t, out, "People on this server", "no roster without candidates")
	require.Contains(t, out, "sends no mail", "copy tells the truth when mail is off")
	require.True(t, strings.Contains(out, `name="role"`), "role picker present")
}

// peopleList is the one surface where a member, a live invite and a dead one
// have to read differently, and each state offers different buttons. Rendering
// it is the only way to catch a row that silently offers Resend on an invite
// that has already expired.
func TestPeopleListPerStateActions(t *testing.T) {
	c := echo.New().NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
	c.Set("csrf", "test-token")
	o := &repo.Org{ID: "o1", Name: "Acme", Slug: "acme"}
	now := time.Now().UTC()
	rows := peopleRows(
		[]repo.OrgMember{{UserID: "u1", Name: "Owner", Email: "o@example.com", Role: "owner"}},
		[]repo.Invite{
			{ID: "tokfresh", Email: "fresh@example.com", Role: "member", ExpiresAt: now.AddDate(0, 0, 14)},
			{ID: "tokstale", Email: "stale@example.com", Role: "viewer", ExpiresAt: now.AddDate(0, 0, -1)},
		},
	)

	var buf bytes.Buffer
	require.NoError(t, peopleList(c, o, rows, "/orgs/acme", true, true).Render(context.Background(), &buf))
	out := buf.String()
	require.Contains(t, out, "/orgs/acme/members/u1/remove", "a member can be removed")
	require.Contains(t, out, "/orgs/acme/invites/tokfresh/resend", "a live invite can be resent")
	require.Contains(t, out, "/orgs/acme/invites/tokstale/reinvite", "an expired invite is reissued, not resent")
	require.NotContains(t, out, "/orgs/acme/invites/tokstale/resend", "resending a dead token would mail a link that fails")
	require.Contains(t, out, "Invite expired", "the expired row says why it has different buttons")

	// Mail off: there is nothing to resend with, so the button must not appear.
	buf.Reset()
	require.NoError(t, peopleList(c, o, rows, "/orgs/acme", false, true).Render(context.Background(), &buf))
	require.NotContains(t, buf.String(), "/resend", "no resend button on a server that sends no mail")

	// A non-owner sees who is in the org and none of the tokens.
	buf.Reset()
	require.NoError(t, peopleList(c, o, rows, "/orgs/acme", true, false).Render(context.Background(), &buf))
	out = buf.String()
	require.Contains(t, out, "Owner", "the member list is not secret")
	require.NotContains(t, out, "tokfresh", "an invite token is a credential")
}

// An owner looking at their own row must not be offered Remove or a role
// select: the server refuses to drop the last owner, and an owner who is not
// the last one can lock themselves out of the org they are configuring.
func TestPeopleListHidesSelfControls(t *testing.T) {
	o := &repo.Org{ID: "o1", Name: "Acme", Slug: "acme"}
	rows := peopleRows([]repo.OrgMember{
		{UserID: "u1", Name: "Owner", Email: "o@example.com", Role: "owner"},
		{UserID: "u2", Name: "Other", Email: "x@example.com", Role: "member"},
	}, nil)

	render := func(viewer *repo.User) string {
		c := echo.New().NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
		c.Set("csrf", "test-token")
		hamrctx.Set(c, hamrctx.SubjectKey, any(viewer))
		var buf bytes.Buffer
		require.NoError(t, peopleList(c, o, rows, "/orgs/acme", true, true).Render(context.Background(), &buf))
		return buf.String()
	}

	out := render(&repo.User{ID: "u1", Name: "Owner"})
	require.NotContains(t, out, "/orgs/acme/members/u1/remove", "own row must not offer Remove")
	require.NotContains(t, out, "Role for Owner", "own row must not offer a role control")
	require.Contains(t, out, "/orgs/acme/members/u2/remove", "everyone else keeps theirs")

	require.Contains(t, render(&repo.User{ID: "u9", Name: "Admin"}),
		"/orgs/acme/members/u1/remove", "someone else's row is still removable")
}

// The org banner is the only thing that tells an owner a plan is waiting, and
// where it points depends on how many are.
func TestOrgPlanBannerTargets(t *testing.T) {
	o := &repo.Org{ID: "o1", Name: "Acme", Slug: "acme"}
	cp := &repo.ConfigPlan{ID: "p1", StackID: "o1", Status: "pending", Summary: "2 changes"}
	render := func(plan *repo.ConfigPlan, n int) string {
		var buf bytes.Buffer
		require.NoError(t, orgPlanBanner(o, plan, n, "/orgs/acme").Render(context.Background(), &buf))
		return buf.String()
	}

	require.NotContains(t, render(nil, 0), "<a", "nothing waiting, no strip")
	one := render(cp, 1)
	require.Contains(t, one, "/orgs/acme/plans/p1", "one plan links straight to it")
	require.Contains(t, one, "2 changes")
	many := render(cp, 3)
	require.Contains(t, many, `href="/orgs/acme/plans"`, "more than one goes to the list")
	require.Contains(t, many, "3 plans awaiting review")
}

// Step 4 for a config-managed org is the same box the other branch asks for,
// read-only, one per declared domain. It used to be a paragraph whose "server
// domain" link went to /servers/local, adminOnly, so a 404 mid-wizard for any
// org owner who is not a server admin.
func TestSetupDomainPageManaged(t *testing.T) {
	c := echo.New().NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
	c.Set("csrf", "test-token")
	managed := &repo.Org{ID: "o1", Name: "Acme", Slug: "acme", ConfigConnectorID: "cn1", ConfigRepo: "acme/infra"}
	res := []repo.DomainResource{{ID: "d1", Host: "acme.example.com"}, {ID: "d2", Host: "acme.dev"}}

	var buf bytes.Buffer
	require.NoError(t, setupDomainPage(c, managed, res, "").Render(context.Background(), &buf))
	out := buf.String()
	for _, r := range res {
		require.Contains(t, out, `value="`+r.Host+`"`, "every declared domain gets a box")
	}
	require.Contains(t, out, "readonly", "the boxes are not editable")
	require.Contains(t, out, "Domains managed by config.")
	require.NotContains(t, out, "/servers/local", "the admin-only link is gone")
	require.NotContains(t, out, `name="host"`, "no add form on the managed branch")

	// The unmanaged branch still asks for one.
	buf.Reset()
	require.NoError(t, setupDomainPage(c, &repo.Org{ID: "o2", Slug: "solo"}, nil, "").Render(context.Background(), &buf))
	out = buf.String()
	require.Contains(t, out, `name="host"`, "an unmanaged org is still asked for a domain")
	require.NotContains(t, out, "managed by config")
}

// The summary used to pick its copy off the placeholder name alone and then
// assume the config branch, so a by-hand draft got "Back to the config file",
// a step that branch does not have: Setup redirects it to this page, and the
// only button on the page led back to the page. Both branches, rendered.
func TestSetupDonePageUnnamedDraftPerBranch(t *testing.T) {
	render := func(mode string) string {
		c := echo.New().NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
		c.Set("csrf", "test-token")
		o := &repo.Org{ID: "o1", Name: setupDraftName, Slug: "org-6447f2", SetupMode: mode}
		var buf bytes.Buffer
		require.NoError(t, setupDonePage(c, o, nil).Render(context.Background(), &buf))
		return buf.String()
	}

	ui := render("ui")
	require.Contains(t, ui, "/orgs/org-6447f2/setup/name", "a by-hand draft is sent to the name step")
	require.NotContains(t, ui, "/orgs/org-6447f2/setup/config", "that step is not on this branch")

	cfg := render("config")
	require.Contains(t, cfg, "/orgs/org-6447f2/setup/config", "the config branch keeps its own way back")
	require.NotContains(t, cfg, "/orgs/org-6447f2/setup/name", "no name step on this branch")
}
