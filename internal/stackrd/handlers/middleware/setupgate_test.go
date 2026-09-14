package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	hamrctx "github.com/FyrmForge/hamr/pkg/ctx"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// The wizard is only a flow if the tabs behind it are shut. Every step links to
// a settings tab, so one click used to eject the owner from onboarding with the
// steps behind them still unfinished and nothing saying so.
func TestRequireSetupDone(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	now := time.Now().UTC()

	org := &repo.Org{ID: "o1", Name: "Acme", Slug: "acme", CreatedAt: now}
	require.NoError(t, store.CreateOrg(ctx, org))
	owner := &repo.User{ID: "u1", Email: "o@example.com", Name: "Owner", Role: "user", Active: true, CreatedAt: now, UpdatedAt: now}
	member := &repo.User{ID: "u2", Email: "m@example.com", Name: "Member", Role: "user", Active: true, CreatedAt: now, UpdatedAt: now}
	for _, u := range []*repo.User{owner, member} {
		require.NoError(t, store.CreateUser(ctx, u))
	}
	require.NoError(t, store.UpsertOrgMember(ctx, &repo.OrgMember{OrgID: org.ID, UserID: owner.ID, Role: "owner", CreatedAt: now}))
	require.NoError(t, store.UpsertOrgMember(ctx, &repo.OrgMember{OrgID: org.ID, UserID: member.ID, Role: "member", CreatedAt: now}))

	call := func(u *repo.User) (int, string, error) {
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/orgs/acme/settings/members", nil), rec)
		c.SetParamNames("slug")
		c.SetParamValues("acme")
		hamrctx.Set(c, hamrctx.SubjectKey, any(u))
		h := stackrmw.RequireSetupDone(store)(func(echo.Context) error {
			return c.String(http.StatusOK, "the settings page")
		})
		err := h(c)
		return rec.Code, rec.Header().Get("Location"), err
	}

	code, loc, err := call(owner)
	require.NoError(t, err)
	require.Equal(t, http.StatusSeeOther, code, "an unfinished org sends its owner back to the wizard")
	require.Equal(t, "/orgs/acme/setup/done", loc, "back to the summary, not to a guessed step: every step is skippable")

	// A member gets the holding page the rest of the org gives them (plan 29),
	// not a 404 on settings and "still being set up" one click away.
	_, _, err = call(member)
	var sp stackrmw.SetupPending
	require.ErrorAs(t, err, &sp, "err = %v", err)
	require.Equal(t, "acme", sp.Slug)

	done := now
	org.SetupDoneAt = &done
	require.NoError(t, store.UpdateOrg(ctx, org))
	code, _, err = call(owner)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code, "a finished org opens its settings")
}

// The settings group was the only thing the wizard closed, so the canvas, the
// plans pages, every stack under the org and the whole API answered normally
// for an org whose setup was never finished. The gate now lives in the access
// check all of those already come through.
func TestRequireOrgAccessGatesUnfinishedOrgs(t *testing.T) {
	now := time.Now().UTC()
	unfinished := repo.Org{ID: "o1", Name: "Acme", Slug: "acme", CreatedAt: now}
	done := unfinished
	done.SetupDoneAt = &now

	call := func(o repo.Org, path string) error {
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
		c.SetPath(path)
		hamrctx.Set(c, hamrctx.SubjectKey, any(&repo.User{ID: "u1", Role: "user"}))
		c.Set(stackrmw.CtxOrgs, []repo.Org{o})
		return stackrmw.RequireOrgAccess(c, o.ID)
	}

	err := call(unfinished, "/orgs/:slug")
	var sp stackrmw.SetupPending
	require.ErrorAs(t, err, &sp, "the org canvas of an unfinished org")
	require.Equal(t, "acme", sp.Slug, "the answer has to name the wizard to send the owner to")

	require.NoError(t, call(unfinished, "/orgs/:slug/setup/:step"),
		"the wizard cannot be gated by its own gate")
	require.NoError(t, call(unfinished, "/orgs/:slug/settings/connectors/:connectorID/branches"),
		"the config step's repo picker runs while setup is open")
	require.NoError(t, call(done, "/orgs/:slug"), "a finished org is open")
}
