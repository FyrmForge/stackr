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
	"github.com/FyrmForge/stackr/internal/stackrd/infra/forward"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/githubapp"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/metrics"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	svcmail "github.com/FyrmForge/stackr/internal/stackrd/service/mail"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	svcproxy "github.com/FyrmForge/stackr/internal/stackrd/service/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	// resources owns the hostnames stackr may generate names under.
	resources  *service.DomainResourceService
	vars       *service.VariableService
	stacks     *service.StackService    // the stack row; a move between orgs is its call
	registries *service.RegistryService // org push/pull credentials and image tags
	members    *service.MemberService   // who is in the org and at what level
	store      repo.Store
	notifier   *notify.Notifier
	sampler    *metrics.Sampler    // traffic lanes on the org canvas; nil in tests
	files      storage.FileStorage // org logos; nil in tests
	rt         *runtime.Runtime    // live forward-relay counts; nil in tests
	forwards   *forward.Registry   // open CLI forward sessions; nil in tests
	orgcfg     *orgconf.Runner     // org config-as-code plans/applies; nil in tests
	gh         *githubapp.Client   // connector repo lists and install state; nil in tests
	mail       *svcmail.Service    // invite emails; nil when no provider is configured
	regsign    *registry.Signer    // managed-registry tokens for the catalog reads; nil in tests
	px         *svcproxy.Service   // re-renders routes when org defaults change; nil in tests
	// work is the durable job runner. An org apply goes on it, never on the
	// request: it creates and binds stacks, each of which plans in turn.
	work *workqueue.Queue
	// settings owns every rung of the defaults cascade.
	settings *service.SettingsService
}

// WithSettings attaches the settings service.
func (h *handler) WithSettings(st *service.SettingsService) *handler { h.settings = st; return h }

// WithWork attaches the durable job runner.
func (h *handler) WithWork(q *workqueue.Queue) *handler { h.work = q; return h }

// WithStacks attaches the stack service, which owns the slug collision a
// move has to refuse.
func (h *handler) WithStacks(st *service.StackService) *handler { h.stacks = st; return h }

func NewHandler(store repo.Store, notifier *notify.Notifier, sampler *metrics.Sampler, files storage.FileStorage, rt *runtime.Runtime, forwards *forward.Registry, orgcfg *orgconf.Runner, gh *githubapp.Client, mailer *svcmail.Service, regsign *registry.Signer, px *svcproxy.Service) *handler {
	return &handler{store: store, notifier: notifier, sampler: sampler, files: files, rt: rt,
		forwards: forwards, orgcfg: orgcfg, gh: gh, mail: mailer, regsign: regsign, px: px,
		// Built here rather than injected: it is stateless, every caller of
		// this constructor has both of its dependencies already, and a nil
		// one would be a silent loss of the membership rules.
		members:    service.NewMemberService(store, mailer, service.NewRevokeService(store, notifier)),
		registries: service.NewRegistryService(store)}
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
	// A move edits BOTH orgs' contents: it takes the stack out of one and
	// puts it in the other, so it needs write rights in each.
	//
	// The route's gate answers for the org the stack is leaving (KindStack on
	// :id). The org it is moving INTO arrives in the form, which the gate
	// cannot see, so that half is checked here and has to stay — see
	// stillBodyGated in handlers/web/gatefree_test.go.
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
	// The form lives on the stack's own settings page, so failures go back
	// there rather than to the org that no longer lists its stacks.
	from, err := h.store.GetOrg(ctx, stack.OrgID)
	if err != nil || from == nil {
		return echo.NewHTTPError(http.StatusNotFound, "org not found")
	}
	if err := h.stacks.Move(ctx, stack, target.ID); err != nil {
		middleware.SetFlash(c, err.Error(), middleware.FlashError)
		return respond.Redirect(c, "/"+from.Slug+"/"+stack.Slug+"/settings")
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

// WithDomainResources gives the page the domain-resource service.
// WithVariables gives the org settings page the variable service.
func (h *handler) WithVariables(v *service.VariableService) *handler { h.vars = v; return h }

func (h *handler) WithDomainResources(r *service.DomainResourceService) *handler {
	h.resources = r
	return h
}
