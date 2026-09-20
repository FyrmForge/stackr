// Package org owns the organization canvas, the top level of the graph, and
// what "/" resolves to. It also holds the few org actions that survive without
// a management page: create one, switch the active one, move a stack between
// them. Members, invites, rename and delete have no UI.
package org

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
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
	stacks     *service.StackService       // the stack row; a move between orgs is its call
	registries *service.RegistryService    // org push/pull credentials and image tags
	members    *service.MemberService      // who is in the org and at what level
	envs       *service.EnvironmentService // a stack's environments, for the canvas
	tiles      *service.TileService        // the tiles the canvas draws
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
	orgs     *service.OrgService
	domains  *service.DomainService
	slices   *service.SliceService
	storage  *service.StorageService
	plans    *service.PlanService
	deploys  *service.DeployService
	audit    *service.AuditService
	auth     *service.AuthService
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
		orgs:       service.NewOrgService(store),
		registries: service.NewRegistryService(store)}
}

// POST /orgs, step 1's answer. Everything this used to decide — one draft
// per person, the placeholder name, the creator's owner row — is
// OrgService.StartDraft now; what is left here is who may ask and where they
// land.
func (h *handler) Create(c echo.Context) error {
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
	o, err := h.orgs.StartDraft(c.Request().Context(), u.ID, c.FormValue("mode"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	h.setActive(c, o.ID)
	return respond.Redirect(c, setupFirstURL(o))
}

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
	target, err := h.orgs.Get(ctx, c.FormValue("org_id"))
	if err != nil {
		return stackrmw.HTTP(err)
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
	stack, err := h.stacks.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	// The form lives on the stack's own settings page, so failures go back
	// there rather than to the org that no longer lists its stacks.
	from, err := h.orgs.Get(ctx, stack.OrgID)
	if err != nil {
		return stackrmw.HTTP(err)
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

// WithTiles gives the canvas the tile service.
func (h *handler) WithTiles(t *service.TileService) *handler { h.tiles = t; return h }

// WithEnvironments gives the canvas the environment service.
func (h *handler) WithEnvironments(e *service.EnvironmentService) *handler { h.envs = e; return h }

func (h *handler) WithDomainResources(r *service.DomainResourceService) *handler {
	h.resources = r
	return h
}

// WithOrgs gives the page the organization service.
func (h *handler) WithOrgs(v *service.OrgService) *handler { h.orgs = v; return h }

// WithMembers gives the page the membership service.
func (h *handler) WithMembers(v *service.MemberService) *handler { h.members = v; return h }

// WithDomains gives the page the domain service.
func (h *handler) WithDomains(v *service.DomainService) *handler { h.domains = v; return h }

// WithSlices gives the page the provision service.
func (h *handler) WithSlices(v *service.SliceService) *handler { h.slices = v; return h }

// WithStorage gives the page the storage service.
func (h *handler) WithStorage(v *service.StorageService) *handler { h.storage = v; return h }

// WithPlans gives the page the config-plan service.
func (h *handler) WithPlans(v *service.PlanService) *handler { h.plans = v; return h }

// WithDeploys gives the page the deploy service.
func (h *handler) WithDeploys(v *service.DeployService) *handler { h.deploys = v; return h }

// WithAudit gives the page the audit trail.
func (h *handler) WithAudit(v *service.AuditService) *handler { h.audit = v; return h }

// WithAuth gives the page the account service, for the user names an org
// settings page shows beside its members.
func (h *handler) WithAuth(a *service.AuthService) *handler { h.auth = a; return h }
