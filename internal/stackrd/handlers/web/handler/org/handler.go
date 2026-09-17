// Package org owns the organization canvas, the top level of the graph, and
// what "/" resolves to. It also holds the few org actions that survive without
// a management page: create one, switch the active one, move a stack between
// them. Members, invites, rename and delete have no UI.
package org

import (
	"net/http"
	"time"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/hamr/pkg/storage"

	"github.com/FyrmForge/stackr/internal/stackrd/config/orgconf"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/forward"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/githubapp"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/mail"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/metrics"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store    repo.Store
	notifier *notify.Notifier
	sampler  *metrics.Sampler    // traffic lanes on the org canvas; nil in tests
	files    storage.FileStorage // org logos; nil in tests
	rt       *runtime.Runtime    // live forward-relay counts; nil in tests
	forwards *forward.Registry   // open CLI forward sessions; nil in tests
	orgcfg   *orgconf.Runner     // org config-as-code plans/applies; nil in tests
	gh       *githubapp.Client   // connector repo lists and install state; nil in tests
	mail     *mail.Mailer        // invite emails; nil when no provider is configured
	regsign  *registry.Signer    // managed-registry tokens for the catalog reads; nil in tests
	px       *proxy.Proxy        // re-renders routes when org defaults change; nil in tests
}

func NewHandler(store repo.Store, notifier *notify.Notifier, sampler *metrics.Sampler, files storage.FileStorage, rt *runtime.Runtime, forwards *forward.Registry, orgcfg *orgconf.Runner, gh *githubapp.Client, mailer *mail.Mailer, regsign *registry.Signer, px *proxy.Proxy) *handler {
	return &handler{store: store, notifier: notifier, sampler: sampler, files: files, rt: rt, forwards: forwards, orgcfg: orgcfg, gh: gh, mail: mailer, regsign: regsign, px: px}
}

// POST /orgs, step 1's answer. The org is created empty and named later, by
// whichever branch the owner picked: the config file names a managed org, the
// name step names a hand-built one. Asking for a name here and letting the file
// overwrite it seconds afterwards is what this replaced.
//
// The creator becomes the org's owner.
func (h *handler) Create(c echo.Context) error {
	ctx := c.Request().Context()
	mode := c.FormValue("mode")
	if mode != "config" && mode != "ui" {
		return echo.NewHTTPError(http.StatusBadRequest, "pick how to set the organization up")
	}
	u := stackrmw.CurrentUser(c)
	if u == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "sign in first")
	}
	// Server admins only, checked here and not just on the route: an org's
	// creator becomes its owner, so an open create is a self-service path to
	// owning something. Not-found rather than forbidden, matching adminOnly.
	if !stackrmw.IsAdmin(c) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	// One draft per person. Coming back to /setup and picking the other branch
	// is the same organization changing its mind, not a second one, matched on
	// "unfinished and owned", not on the placeholder name, because the UI branch
	// renames at its very first step.
	if o := h.unfinishedDraft(c, u.ID); o != nil {
		o.SetupMode = mode
		if err := h.store.UpdateOrg(ctx, o); err != nil {
			return err
		}
		h.setActive(c, o.ID)
		return respond.Redirect(c, setupFirstURL(o))
	}
	o := &repo.Org{
		ID:        uuid.New().String(),
		Name:      setupDraftName,
		Slug:      draftSlug(),
		CreatedAt: time.Now().UTC(),
		SetupMode: mode,
	}
	if err := h.store.CreateOrg(ctx, o); err != nil {
		return err
	}
	if err := h.store.UpsertOrgMember(ctx, &repo.OrgMember{
		OrgID: o.ID, UserID: u.ID, Role: "owner", CreatedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	h.setActive(c, o.ID)
	return respond.Redirect(c, setupFirstURL(o))
}

// unfinishedDraft is the org this user is already halfway through setting up,
// if there is one. Nil is the normal answer.
//
// The newest wins. ListOrgsForUser orders by name, so taking the first match
// would make "the draft" a store-order accident the moment someone owns two,
// an org they were invited to as owner and never finished, say.
func (h *handler) unfinishedDraft(c echo.Context, userID string) *repo.Org {
	orgs, err := h.store.ListOrgsForUser(c.Request().Context(), userID)
	if err != nil {
		return nil
	}
	ctx := c.Request().Context()
	var newest *repo.Org
	for i := range orgs {
		if orgs[i].SetupDoneAt != nil {
			continue
		}
		// The caller's own role, not ownerOf: that one says yes to any admin,
		// which would make a stranger's half-finished org, one this admin was
		// invited to as a viewer, the draft their next answer to step 1 moves.
		m, err := h.store.GetOrgMember(ctx, orgs[i].ID, userID)
		if err != nil || m == nil || m.Role != "owner" {
			continue
		}
		if newest == nil || orgs[i].CreatedAt.After(newest.CreatedAt) {
			newest = &orgs[i]
		}
	}
	return newest
}

// draftSlug is a URL for an org with no name yet. Random rather than counted:
// two people starting at once must not collide on the same slug.
func draftSlug() string { return "org-" + uuid.New().String()[:6] }

// POST /orgs/switch, only into orgs the user belongs to.
func (h *handler) Switch(c echo.Context) error {
	id := c.FormValue("id")
	if !stackrmw.InOrg(c, id) {
		return echo.NewHTTPError(http.StatusNotFound, "org not found")
	}
	h.setActive(c, id)
	return respond.Redirect(c, "/")
}

// POST /orgs/stacks/:id/move
func (h *handler) MoveStack(c echo.Context) error {
	ctx := c.Request().Context()
	target, err := h.store.GetOrg(ctx, c.FormValue("org_id"))
	if err != nil {
		return err
	}
	// A move edits both orgs' contents, it takes the stack out of one and
	// puts it in the other, so it needs write rights in each, in that org
	// rather than in whichever one the cookie has selected.
	if target == nil {
		return echo.NewHTTPError(http.StatusNotFound, "org not found")
	}
	if err := stackrmw.RequireOrgWrite(c, h.store, target.ID); err != nil {
		return err
	}
	stack, err := h.store.GetStack(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	if stack == nil {
		return echo.NewHTTPError(http.StatusNotFound, "stack not found")
	}
	if err := stackrmw.RequireOrgWrite(c, h.store, stack.OrgID); err != nil {
		return err
	}
	// The form lives on the stack's own settings page, so failures go back
	// there rather than to the org that no longer lists its stacks.
	from, err := h.store.GetOrg(ctx, stack.OrgID)
	if err != nil || from == nil {
		return echo.NewHTTPError(http.StatusNotFound, "org not found")
	}
	if other, _ := h.store.GetStackBySlug(ctx, target.ID, stack.Slug); other != nil {
		middleware.SetFlash(c, "That org already has a stack with this slug.", middleware.FlashError)
		return respond.Redirect(c, "/"+from.Slug+"/"+stack.Slug+"/settings")
	}
	if err := h.store.SetStackOrg(ctx, stack.ID, target.ID); err != nil {
		return err
	}
	middleware.SetFlash(c, "Moved "+stack.Name+" to "+target.Name+". Its connector-based clones now use "+target.Name+"'s connectors.", middleware.FlashSuccess)
	return respond.Redirect(c, "/"+target.Slug+"/"+stack.Slug+"/settings")
}

func (h *handler) setActive(c echo.Context, id string) {
	c.SetCookie(&http.Cookie{
		Name:     stackrmw.OrgCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		HttpOnly: true,
		Secure:   stackrmw.SecureCookie(c),
		SameSite: http.SameSiteLaxMode,
	})
}
