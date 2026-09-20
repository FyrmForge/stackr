package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	neturl "net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/hamr/pkg/htmx"
	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/config/runpolicy"
	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/config/sharelink"
	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/config/staging"
	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/envcolor"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/avatar"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components/canvas"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/graph"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/forward"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/githubapp"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/metrics"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/placement"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/volmove"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	svcproxy "github.com/FyrmForge/stackr/internal/stackrd/service/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/audit"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	// resources owns the hostnames stackr may generate names under.
	resources *service.DomainResourceService
	store     repo.Store
	rt        *runtime.Runtime
	clus      *cluster.Cluster
	px        *svcproxy.Service
	sampler   *metrics.Sampler
	notifier  *notify.Notifier
	gh        *githubapp.Client
	applier   stackconf.Applier
	forwards  *forward.Registry // live tunnel sessions; nil in tests
	// sched re-registers the cron and backup tables after a write that
	// changes or cascades their rows.
	sched *scheduler.Service
	// tiles owns the tile row: create, delete and the cascade around each.
	tiles *service.TileService
	vars  *service.VariableService
	envs  *service.EnvironmentService
	// stacks owns the stack row: create, rename, delete, bind.
	stacks *service.StackService
	// deploys owns which tiles may be deployed; releases owns the ladder;
	// plans owns a config plan after it exists.
	deploys  *service.DeployService
	releases *service.ReleaseService
	plans    *service.PlanService
	// prenvs owns the pull-request environment settings.
	prenvs    *service.PREnvService
	instances *service.ManagedInstanceService
	// settings owns every rung of the defaults cascade.
	settings *service.SettingsService
	// domains owns the hostnames a tile answers on.
	domains *service.DomainService
	// mover is the volume-move service, so a card being moved can show it.
	// nil in tests and on installs that have never added a node.
	mover *volmove.Service
	// work is the durable job runner. An apply is enqueued on it, never run on
	// the request: it clones repos and builds images, so it routinely outlives
	// the browser that asked for it.
	work *workqueue.Queue
}

// WithMover attaches the volume-move service. Set from the router rather than
// passed to NewHandler, whose signature is already nine arguments long and is
// called from tests that have no swarm at all.
func (h *handler) WithMover(m *volmove.Service) *handler { h.mover = m; return h }

func (h *handler) WithWork(q *workqueue.Queue) *handler { h.work = q; return h }

// WithSettings attaches the settings service.
func (h *handler) WithSettings(st *service.SettingsService) *handler { h.settings = st; return h }

// NewHandler creates a new project handler.
func NewHandler(store repo.Store, rt *runtime.Runtime, clus *cluster.Cluster, px *svcproxy.Service, sampler *metrics.Sampler, notifier *notify.Notifier, gh *githubapp.Client, applier stackconf.Applier, forwards *forward.Registry) *handler {
	return &handler{store: store, rt: rt, clus: clus, px: px, sampler: sampler, notifier: notifier, gh: gh, applier: applier, forwards: forwards}
}

// WithScheduler gives the canvas the schedule reloader.
func (h *handler) WithScheduler(s *scheduler.Service) *handler { h.sched = s; return h }

// WithTiles gives the canvas the tile service, the same value the app page
// and the API hold.
func (h *handler) WithTiles(t *service.TileService) *handler { h.tiles = t; return h }

// WithVariables gives the canvas the variable service.
func (h *handler) WithVariables(v *service.VariableService) *handler { h.vars = v; return h }

// WithEnvironments gives the canvas the environment service.
func (h *handler) WithEnvironments(e *service.EnvironmentService) *handler { h.envs = e; return h }

// WithStacks attaches the stack service.
func (h *handler) WithStacks(st *service.StackService) *handler { h.stacks = st; return h }

// WithPREnvs attaches the pull-request environment settings service.
func (h *handler) WithPREnvs(p *service.PREnvService) *handler { h.prenvs = p; return h }

// WithPlans attaches the plan service.
func (h *handler) WithPlans(p *service.PlanService) *handler { h.plans = p; return h }

// WithDeploys attaches the deploy service.
func (h *handler) WithDeploys(d *service.DeployService) *handler { h.deploys = d; return h }

// WithReleases attaches the release service, which owns the promotion ladder.
func (h *handler) WithReleases(r *service.ReleaseService) *handler { h.releases = r; return h }

// WithInstances gives the canvas the managed-instance service.
func (h *handler) WithInstances(m *service.ManagedInstanceService) *handler {
	h.instances = m
	return h
}

// WithDomains gives the canvas the domain service.
func (h *handler) WithDomains(d *service.DomainService) *handler { h.domains = d; return h }

// loadStack fetches a stack by id with the org tenancy check applied.
func (h *handler) loadStack(c echo.Context, id string) (*repo.Stack, error) {
	p, err := h.stacks.Get(c.Request().Context(), id)
	if err != nil {
		return nil, stackrmw.HTTP(err)
	}
	// Every redirect built from stackURL needs the org slug; without it the
	// URL carries the org id and 404s (approve / plan-again did exactly that).
	h.fillOrg(c.Request().Context(), p)
	return p, nil
}

// GET /projects/:id/repos, <option>s for the create-modal repo picker,
// aggregated across the stack org's connected GitHub connectors.
// Option value: "<connectorID>|<git url>|<default branch>".
func (h *handler) Repos(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	type pick struct{ Value, Label string }
	var picks []pick
	conns, _ := h.store.ListConnectorsByOrg(ctx, p.OrgID)
	for i := range conns {
		if conns[i].Provider != "github" || !githubapp.ParseConfig(conns[i].Config).Connected() {
			continue
		}
		repos, err := h.gh.ListRepos(ctx, &conns[i])
		if err != nil {
			c.Logger().Warnf("github repo list (%s): %v", conns[i].Name, err)
			continue
		}
		for _, r := range repos {
			picks = append(picks, pick{
				Value: conns[i].ID + "|https://github.com/" + r.FullName + "|" + r.DefaultBranch,
				Label: r.FullName,
			})
		}
	}
	var b strings.Builder
	for _, pk := range picks {
		b.WriteString(`<option value="` + html.EscapeString(pk.Value) + `">` + html.EscapeString(pk.Label) + `</option>`)
	}
	return c.HTML(http.StatusOK, b.String())
}

// POST /projects
func (h *handler) Create(c echo.Context) error {
	name := c.FormValue("name")
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name required")
	}
	// No fallback org: a fresh install has none until the setup flow creates
	// one, and every stack must live in an org the user chose.
	org, ok := stackrmw.ActiveOrg(c)
	if !ok {
		return echo.NewHTTPError(http.StatusBadRequest, "create an organization first")
	}
	orgID := org.ID
	// A create resolves its org from the active-org cookie rather than from a
	// resource, so it is the one write that misses RequireOrgAccess, and with
	// it the gate that keeps an unfinished org from being filled in sideways.
	if err := stackrmw.RequireOrgWrite(c, h.store, orgID); err != nil {
		return err
	}
	// The duplicate slug is the one that bit: this path let UNIQUE
	// (org_id, slug) refuse it, which reached the user as a raw 500.
	p, err := h.stacks.Create(c.Request().Context(), orgID, service.CreateStack{Name: name})
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return respond.Redirect(c, "/projects/"+p.ID)
}

// defaultEnv returns the stack's oldest environment, the pre-multi-env UI
// operates on this one. Phase 3 (env switcher) replaces callers with an
// explicit env from the URL.
func (h *handler) defaultEnv(ctx context.Context, stackID string) (*repo.Environment, error) {
	envs, err := h.envs.ListForStack(ctx, stackID)
	if err != nil {
		return nil, err
	}
	if len(envs) == 0 {
		return nil, echo.NewHTTPError(http.StatusNotFound, "stack has no environments")
	}
	return &envs[0], nil
}

// envFromForm picks the environment a create-form targets: explicit env_id
// when present, the stack's default env otherwise.
func (h *handler) envFromForm(c echo.Context, stackID string) (*repo.Environment, error) {
	if id := c.FormValue("env_id"); id != "" {
		env, err := h.store.GetEnvironment(c.Request().Context(), id)
		if err != nil {
			return nil, err
		}
		if env == nil || env.StackID != stackID {
			return nil, echo.NewHTTPError(http.StatusBadRequest, "invalid environment")
		}
		return env, nil
	}
	return h.defaultEnv(c.Request().Context(), stackID)
}

// fillOrg populates p.OrgSlug for URL building (envURL/stackSettingsURL).
func (h *handler) fillOrg(ctx context.Context, p *repo.Stack) {
	if p == nil || p.OrgSlug != "" {
		return
	}
	if org, _ := h.store.GetOrg(ctx, p.OrgID); org != nil {
		p.OrgSlug = org.Slug
	}
}

// settingsURL fills the org slug and returns the stack settings path.
func (h *handler) settingsURL(ctx context.Context, p *repo.Stack) string {
	h.fillOrg(ctx, p)
	return stackSettingsURL(p)
}

// settingsSection returns the URL of one settings section, so a save lands
// back where it was made instead of at the top of the page.
func (h *handler) settingsSection(ctx context.Context, p *repo.Stack, section string) string {
	return h.settingsURL(ctx, p) + "/" + section
}

// envSettingsURL is one environment's own settings page.
func (h *handler) envSettingsURL(ctx context.Context, p *repo.Stack, envSlug string) string {
	return h.settingsSection(ctx, p, "environments") + "/" + envSlug
}

// resolveSlugs maps /:org/:stack/:env path params to rows.
func (h *handler) resolveSlugs(c echo.Context) (*repo.Stack, *repo.Environment, error) {
	ctx := c.Request().Context()
	org, err := h.store.GetOrgBySlug(ctx, c.Param("org"))
	if err != nil {
		return nil, nil, err
	}
	if org == nil {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "org not found")
	}
	p, err := h.stacks.BySlug(ctx, org.ID, c.Param("stack"))
	if err != nil {
		return nil, nil, stackrmw.HTTP(err)
	}
	p.OrgSlug = org.Slug
	env, err := h.envs.BySlug(ctx, p.ID, c.Param("env"))
	if err != nil {
		return nil, nil, stackrmw.HTTP(err)
	}
	return p, env, nil
}

// RedirectStack sends an id-based stack URL to the canonical slug URL of its
// stack canvas. The env graph is one level down from there, nothing lands
// straight in an environment any more.
func (h *handler) RedirectStack(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.stacks.Get(ctx, c.Param("id"))
	if errors.Is(err, svcerr.ErrNotFound) {
		// /:org/:stack form, resolve by slugs.
		org, oerr := h.store.GetOrgBySlug(ctx, c.Param("org"))
		if oerr != nil || org == nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		p, err = h.stacks.BySlug(ctx, org.ID, c.Param("stack"))
	}
	if err != nil {
		return stackrmw.HTTP(err)
	}
	h.fillOrg(ctx, p)
	return c.Redirect(http.StatusSeeOther, stackURL(p))
}

// CreateEnvironment adds an environment to a stack, empty, or a config-only
// clone of another env (base_env_id). type=ephemeral marks it disposable.
// POST /projects/:id/envs
func (h *handler) CreateEnvironment(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	env, err := h.envs.Create(ctx, p, service.CreateEnv{
		Name: c.FormValue("name"), Type: c.FormValue("type"),
		BaseEnvID: c.FormValue("base_env_id"),
	}, stackrmw.WebActor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Environment created. Nothing is deployed yet. Deploy tiles when ready.", middleware.FlashSuccess)
	return respond.Redirect(c, h.envSettingsURL(ctx, p, env.Slug))
}

// DeleteEnvironment tears an environment down: containers, proxy routes, the
// env network, then the rows (tiles cascade). The last environment of a
// stack cannot be deleted. DB volumes are kept, matching single-db deletes.
// POST /envs/:id/delete
func (h *handler) DeleteEnvironment(c echo.Context) error {
	ctx := c.Request().Context()
	env, err := h.envs.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	p, err := h.loadStack(c, env.StackID)
	if err != nil {
		return err
	}
	if err := components.RequireConfirm(c, env.Slug); err != nil {
		return err
	}
	// Typing the slug to confirm is this surface's force: the dialog spells
	// out what goes, so the running check has already been answered by a
	// person. The API and the CLI take a flag instead.
	if err := h.envs.Delete(ctx, p, env, true, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Environment deleted. DB volumes were kept.", middleware.FlashSuccess)
	return respond.Redirect(c, h.settingsSection(ctx, p, "environments"))
}

// ResetEnvironment tears a config-managed stack's environment down so the next
// apply rebuilds it from the file. It exists because a half-applied env had no
// way out inside the product: env delete is refused on a config-managed stack
// (the file is the source of truth), tile delete is refused for the same
// reason, and a generated hostname taken by the wrong env cannot be released
// from the config file at all. The only route out was editing the database by
// hand.
//
// Not a delete: the file still declares this environment, so the row coming
// back is the point. Tiles, containers, proxy routes and the env network go.
// DB volumes are kept, matching every other teardown here.
// POST /envs/:id/reset
func (h *handler) ResetEnvironment(c echo.Context) error {
	ctx := c.Request().Context()
	env, err := h.envs.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	p, err := h.loadStack(c, env.StackID)
	if err != nil {
		return err
	}
	if err := components.RequireConfirm(c, env.Slug); err != nil {
		return err
	}
	// Confirmed by typing the slug, so the running check is already answered.
	if err := h.envs.Reset(ctx, p, env, true, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	// Re-planned again, synchronously: the service kicks a background replan,
	// but this page wants the new plan to exist before it redirects, or the
	// stale diff would read as "finish what broke" when it is really "build
	// the whole thing again".
	if _, err := h.planner().RunAll(ctx, p, ""); err != nil && err != stackconf.ErrNoFile {
		middleware.SetFlash(c, "Environment reset, but re-planning failed: "+err.Error(), middleware.FlashError)
		return respond.Redirect(c, h.settingsSection(ctx, p, "environments"))
	}
	middleware.SetFlash(c, "Environment "+env.Slug+" was torn down. Apply the plan to build it again.", middleware.FlashSuccess)
	return respond.Redirect(c, h.settingsSection(ctx, p, "environments"))
}

// POST /projects/:id/delete
func (h *handler) Delete(c echo.Context) error {
	// loadStack enforces org access (was missing, any user could delete any
	// stack by ID). Deleting a config-managed stack is allowed on purpose: it
	// removes the config binding with it, so no plan can revert it.
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := components.RequireConfirm(c, p.Slug); err != nil {
		return err
	}
	if err := h.stacks.Delete(c.Request().Context(), p); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Stack deleted.", middleware.FlashSuccess)
	return respond.Redirect(c, "/")
}

// POST /projects/:id/apps, create an app in this project.
//
// Binds the form onto a fresh row and hands it to the tile service: the slug,
// the cron expression, the source, the connector's org, the volume's attach
// target and the decision to stage are all rules, and rules are not this
// function's business.
func (h *handler) CreateTile(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	sourceType := c.FormValue("source_type")
	switch sourceType {
	case "git", "image":
	default:
		sourceType = "git"
	}
	kind := c.FormValue("kind")
	switch kind {
	case "cron", "function", "volume":
	default:
		kind = "service"
	}
	if kind == "volume" {
		// A volume has no build of its own; the field is not on its form.
		sourceType = "image"
	}
	env, err := h.envFromForm(c, p.ID)
	if err != nil {
		return err
	}
	a := &repo.Tile{
		StackID:        p.ID,
		EnvironmentID:  env.ID,
		Name:           c.FormValue("name"),
		Kind:           kind,
		Cron:           c.FormValue("cron"),
		Command:        c.FormValue("command"),
		RunOnDeploy:    kind == "function" && c.FormValue("run_on_deploy") != "",
		ImageRef:       c.FormValue("image_ref"),
		SourceType:     sourceType,
		ContainerPort:  atoiOr(c.FormValue("container_port"), 0),
		GitURL:         strings.TrimSpace(c.FormValue("git_url")),
		GitBranch:      c.FormValue("git_branch"),
		ConnectorID:    c.FormValue("connector_id"),
		AttachedTileID: c.FormValue("attach_tile_id"),
		MountPath:      strings.TrimSpace(c.FormValue("mount_path")),
	}
	// The staged payload is a full config object, which is the config
	// engine's shape and so this handler's to build. Only the canvas ever
	// stages a create, so only the canvas builds one.
	var patch any
	if !p.ConfigManaged() {
		tc := stackconf.TileConfOf(stackconf.TileState{Tile: *a}, stackconf.EnvState{})
		if kind == "volume" {
			tc.Path = a.MountPath
			if target, terr := h.tiles.Get(ctx, a.AttachedTileID); terr == nil {
				tc.Attach = target.Slug
			}
		}
		patch = tc
	}
	staged, err := h.tiles.Create(ctx, a, patch, stackrmw.WebActor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if staged {
		// Nothing exists until the pending set is applied.
		middleware.SetFlash(c, "Tile creation staged. Review & apply on the canvas to deploy.", middleware.FlashSuccess)
		h.fillOrg(ctx, p)
		return respond.Redirect(c, envURL(p, env))
	}
	if c.FormValue("from") == "canvas" {
		h.fillOrg(ctx, p)
		return respond.Redirect(c, envURL(p, env))
	}
	return respond.Redirect(c, "/apps/"+a.ID)
}

// Graph renders one environment's topology canvas (slug route
// /:org/:stack/:env; id routes redirect here).
func (h *handler) Graph(c echo.Context) error {
	ctx := c.Request().Context()
	p, env, err := h.resolveSlugs(c)
	if err != nil {
		return err
	}
	envs, err := h.envs.ListForStack(ctx, p.ID)
	if err != nil {
		return err
	}
	g, err := h.buildGraph(ctx, env.ID, arrangeStyle(c))
	if err != nil {
		return err
	}
	pendingPlan, _ := h.store.LatestConfigPlan(ctx, p.ID)
	if pendingPlan != nil && pendingPlan.Status != "pending" && pendingPlan.Status != "error" {
		pendingPlan = nil
	}
	stagedCount, _ := h.store.CountStagedByEnv(ctx, env.ID)
	cmp, _, err := h.compareStack(ctx, p)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, graphPage(c, p, env, envs, h.envColorsByID(ctx, p, envs), g, pendingPlan, stagedCount, cmp, h.commitLog(ctx, p)))
}

// envColorsByID resolves every env's colour for one stack, as CSS values.
func (h *handler) envColorsByID(ctx context.Context, p *repo.Stack, envs []repo.Environment) map[string]string {
	org, _ := h.store.GetOrg(ctx, p.OrgID)
	out := map[string]string{}
	for id, r := range envcolor.Map(envs, org, p.ConfigManaged()) {
		out[id] = r.CSS
	}
	return out
}

// envColorsBySlug is envColorsByID keyed by slug, for plan rows.
func (h *handler) envColorsBySlug(ctx context.Context, p *repo.Stack) map[string]string {
	envs, err := h.envs.ListForStack(ctx, p.ID)
	if err != nil {
		return nil
	}
	byID := h.envColorsByID(ctx, p, envs)
	out := make(map[string]string, len(envs))
	for _, e := range envs {
		out[e.Slug] = byID[e.ID]
	}
	return out
}

// planner is the config-as-code planner shared with the applier.
func (h *handler) planner() stackconf.Planner {
	return h.applier.Planner
}

// engine is the deploy engine shared with the applier.
func (h *handler) engine() *deploy.Engine { return h.applier.Engine }

// managedErr is the 409 returned when a structural write is blocked because the
// config file owns the stack, a config plan would revert it (the silent-drift
// bug). Secrets, certs and provisioning are not structural and don't use this.
var managedErr = stackrmw.ManagedErr

// stageChange records a pending change for a tile, stamped with the current
// user (mirror of the app handler's stage helper).
func (h *handler) stageChange(c echo.Context, t *repo.Tile, summary, op string, patch any) error {
	authorID, authorName := "", ""
	if u := stackrmw.CurrentUser(c); u != nil {
		authorID, authorName = u.ID, u.Name
	}
	return staging.Stage(c.Request().Context(), h.store, t, authorID, authorName, summary, op, patch)
}

// POST /projects/:id/config, bind (or unbind) the stack's config repo.
func (h *handler) SaveConfigBinding(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	if c.FormValue("connector_id") == "" {
		if err := h.stacks.Unbind(ctx, p); err != nil {
			return stackrmw.HTTP(err)
		}
		middleware.SetFlash(c, "Config repo unbound. Stack is UI-managed again.", middleware.FlashSuccess)
		return respond.Redirect(c, h.settingsSection(ctx, p, "config"))
	}
	// The connector-in-org check, the repo normalisation, the staged drop and
	// the plan run all live in the service now: a config apply binding the
	// same stack did none of them.
	if err := h.stacks.Bind(ctx, p, service.BindConfig{
		ConnectorID: c.FormValue("connector_id"),
		Repo:        c.FormValue("repo"),
		Branch:      c.FormValue("branch"),
		Path:        c.FormValue("path"),
	}); err != nil {
		return stackrmw.HTTP(err)
	}
	h.runPlan(c, p)
	return respond.Redirect(c, h.settingsSection(ctx, p, "config"))
}

// POST /projects/:id/config/plan, manual re-plan.
func (h *handler) PlanNow(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	plans := h.runPlan(c, p)
	// From a plan page the answer is the new plan, not the settings tab.
	if c.FormValue("plan_only") != "" && len(plans) > 0 {
		return respond.Redirect(c, stackURL(p)+"/plans/"+plans[0].ID)
	}
	return respond.Redirect(c, h.settingsSection(ctx, p, "config"))
}

func (h *handler) runPlan(c echo.Context, p *repo.Stack) []*repo.ConfigPlan {
	ctx := c.Request().Context()
	// Through the service, for the binding check this path never had: on an
	// unbound stack it ran a planner that could only fail and reported the
	// failure as a plan.
	plans, err := h.plans.Run(ctx, p)
	switch {
	case err == stackconf.ErrNoFile:
		middleware.SetFlash(c, "No config file found in the bound repo. Nothing planned.", middleware.FlashError)
	case err != nil:
		middleware.SetFlash(c, "Plan failed: "+err.Error(), middleware.FlashError)
	case len(plans) == 0:
		middleware.SetFlash(c, "Nothing planned.", middleware.FlashError)
	case plans[0].Status == "error":
		middleware.SetFlash(c, "Config invalid: "+plans[0].Error, middleware.FlashError)
	default:
		middleware.SetFlash(c, "Plan: "+plans[0].Summary, middleware.FlashSuccess)
	}
	return plans
}

// POST /projects/:id/config/plans/:planID/approve, apply a pending plan
// (deletes included; per-env policy is bypassed by explicit approval).
func (h *handler) ApprovePlan(c echo.Context) error {
	ctx := c.Request().Context()
	p, cp, err := h.loadPlan(c)
	if err != nil {
		return err
	}
	// Enqueued, not run here. An apply clones repos and builds images, so it
	// routinely outlives the request; on the request's context it died wherever
	// it had got to and the failure was written through the same dead context
	// and lost. The pending check is the service's, and answers 409 on both
	// surfaces now — two people pressing Apply is a race, not a bad request.
	if _, err := h.plans.Approve(ctx, p, cp); err != nil {
		middleware.SetFlash(c, "Could not queue the apply: "+err.Error(), middleware.FlashError)
		return respond.Redirect(c, stackURL(p)+"/plans/"+cp.ID)
	}
	middleware.SetFlash(c, "Applying. This page follows it.", middleware.FlashSuccess)
	// The plan page, not the canvas: that is where the progress and the
	// outcome show up now. The return path rides along so its own Back works.
	to := stackURL(p) + "/plans/" + cp.ID
	if ret := localPath(c.FormValue("return")); ret != "" {
		to += "?return=" + neturl.QueryEscape(ret)
	}
	return respond.Redirect(c, to)
}

// POST /projects/:id/config/plans/:planID/reject
func (h *handler) RejectPlan(c echo.Context) error {
	ctx := c.Request().Context()
	p, cp, err := h.loadPlan(c)
	if err != nil {
		return err
	}
	if err := h.plans.Reject(ctx, cp); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Plan rejected.", middleware.FlashSuccess)
	if ret := localPath(c.FormValue("return")); ret != "" {
		return respond.Redirect(c, ret)
	}
	return respond.Redirect(c, h.settingsSection(ctx, p, "config"))
}

func (h *handler) loadPlan(c echo.Context) (*repo.Stack, *repo.ConfigPlan, error) {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return nil, nil, err
	}
	cp, err := h.store.GetConfigPlan(ctx, c.Param("planID"))
	if err != nil || cp == nil || cp.StackID != p.ID {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "plan not found")
	}
	return p, cp, nil
}

// POST /envs/:id/config, per-env config branch + apply policy.
func (h *handler) SaveEnvConfig(c echo.Context) error {
	ctx := c.Request().Context()
	env, err := h.envs.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	p, err := h.loadStack(c, env.StackID)
	if err != nil {
		return err
	}
	// The form is shown (disabled) on unmanaged stacks so the setting is
	// discoverable before a repo is bound; the route refuses the write to match.
	if !p.ConfigManaged() {
		return echo.NewHTTPError(http.StatusConflict, "bind a config repository first")
	}
	// SP1: an apply policy the form did not offer is refused, not folded to
	// "". This coerced, so a stale or hand-posted value silently reset the
	// policy to inherit.
	branch := strings.TrimSpace(c.FormValue("config_branch"))
	policy := c.FormValue("apply_policy")
	if err := h.envs.Update(ctx, env, service.EnvPatch{
		ApplyPolicy: &policy, ConfigBranch: &branch,
	}, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	if env.ConfigBranch != "" && p.ConfigManaged() {
		if _, err := h.planner().RunEnv(ctx, p, env, ""); err != nil && err != stackconf.ErrNoFile {
			middleware.SetFlash(c, "Saved, but planning failed: "+err.Error(), middleware.FlashError)
			return respond.Redirect(c, h.envSettingsURL(ctx, p, env.Slug))
		}
	}
	middleware.SetFlash(c, "Environment config settings saved.", middleware.FlashSuccess)
	return respond.Redirect(c, h.envSettingsURL(ctx, p, env.Slug))
}

// stackPlanCfg points the shared plan view at this stack's endpoints. The
// return-to path rides the button URLs rather than a hidden field, so the
// component needs to know nothing about it.
// stackPlanWorkCfg is stackPlanCfg plus the apply banner, which the plan page
// polls and the canvas's own plan links do not.
func stackPlanWorkCfg(p *repo.Stack, cp *repo.ConfigPlan, work *repo.WorkItem, returnURL, envColor string) components.PlanViewCfg {
	cfg := stackPlanCfg(p, cp, returnURL, envColor)
	cfg.Work, cfg.PollURL = work, "/projects/"+p.ID+"/config/plans/"+cp.ID
	return cfg
}

func stackPlanCfg(p *repo.Stack, cp *repo.ConfigPlan, returnURL, envColor string) components.PlanViewCfg {
	cfg := components.PlanViewCfg{
		Summary:   cp.Summary,
		Commit:    cp.CommitSHA,
		When:      cp.CreatedAt.Local().Format("Jan 2 15:04:05"),
		EnvSlug:   cp.EnvSlug,
		EnvColor:  envColor,
		Status:    cp.Status,
		StoredErr: cp.Error,
		ReplanURL: "/projects/" + p.ID + "/config/plan",
	}
	base := "/projects/" + p.ID + "/config/plans/" + cp.ID
	q := ""
	if returnURL != "" {
		q = "?return=" + neturl.QueryEscape(returnURL)
	}
	if cp.Status != "pending" {
		return cfg
	}
	cfg.ApproveURL = base + "/approve" + q
	cfg.RejectURL = base + "/reject" + q
	cfg.InputURL = base + "/inputs" + q
	return cfg
}

// SetPlanInput fills in a value the config declares and nobody has set, from
// the plan page itself, then re-plans so the diff on screen is the one being
// approved. The runner supersedes the previous pending plan.
func (h *handler) SetPlanInput(c echo.Context) error {
	ctx := c.Request().Context()
	p, _, err := h.loadPlan(c)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(c.FormValue("name"))
	if !varNameOK(name) {
		return echo.NewHTTPError(http.StatusBadRequest, "variable name: letters, digits, _ . - only")
	}
	// An empty value would count as set: the row leaves the plan and the tiles
	// waiting on it are released, only to deploy with a blank credential.
	val := c.FormValue("value")
	if val == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "a value is required")
	}
	// Set, not the service's own replan: this page wants the *new* plan in
	// hand to redirect to, so it runs one synchronously below instead.
	if err := h.vars.Set(ctx, service.StackVars(p.ID), []service.VarWrite{{
		Name: name, Value: val, Secret: true,
	}}, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, name+" set.", middleware.FlashSuccess)
	h.fillOrg(ctx, p)
	plans, perr := h.planner().RunAll(ctx, p, "")
	if perr != nil || len(plans) == 0 {
		return respond.Redirect(c, h.settingsSection(ctx, p, "config"))
	}
	to := stackURL(p) + "/plans/" + plans[0].ID
	if ret := localPath(c.FormValue("return")); ret != "" {
		to += "?return=" + neturl.QueryEscape(ret)
	}
	return respond.Redirect(c, to)
}

// GET /projects/:id/config/plans/:planID, read-only plan diff view.
func (h *handler) PlanView(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.resolveStackSlugs(c)
	if err != nil {
		return err
	}
	cp, err := h.store.GetConfigPlan(ctx, c.Param("planID"))
	if err != nil || cp == nil || cp.StackID != p.ID {
		return echo.NewHTTPError(http.StatusNotFound, "plan not found")
	}
	var plan stackconf.Plan
	_ = json.Unmarshal([]byte(cp.Plan), &plan)
	// The apply runs on the work queue now, so the page has to say where it
	// got to. Keyed on the plan, which is the key the enqueue uses: this asked
	// for the stack's id, so it never matched a row and the banner never
	// rendered at all. An older plan's page cannot narrate a newer plan's
	// apply, because the key is the plan.
	work, _ := h.store.LatestWorkItem(ctx, stackconf.ApplyKind, cp.ID)
	// Move blocks are re-checked against live placement, then named.
	//
	// Re-checked because the stored plan is a snapshot: once the operator
	// has done the move the block it recorded is history, and leaving it in
	// kept "Approve & apply" disabled with nothing on the page to say the
	// plan had to be re-planned first. An apply re-diffs
	// from the commit anyway, so a stale block only ever blocked the button.
	//
	// Named because the block carries a swarm node id: the planner has no
	// swarm connection and must not grow one, but this page has the store.
	live := plan.Moves[:0]
	for i := range plan.Moves {
		m := plan.Moves[i]
		if t, terr := h.tiles.Get(ctx, m.TileID); terr == nil &&
			placement.InGroup(ctx, h.store, h.rt, t, m.ToGroup) {
			continue // the data is already on a node in the wanted group
		}
		if sv, serr := h.store.GetServerByNodeID(ctx, m.FromNode); serr == nil && sv != nil {
			m.FromNode = sv.Name
		}
		live = append(live, m)
	}
	plan.Moves = live
	return respond.HTML(c, http.StatusOK, planPage(c, p, cp, &plan, work,
		h.settingsSection(ctx, p, "config"), localPath(c.QueryParam("return")), h.envColorsBySlug(ctx, p)[cp.EnvSlug]))
}

// GET /projects/:id/config/plans/:planID, the pre-rename URL. Stacks were
// called projects, and links to this one are already out there (banners,
// flashes, bookmarks), so it stays as a redirect rather than a 404.
func (h *handler) PlanRedirect(c echo.Context) error {
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	h.fillOrg(c.Request().Context(), p)
	to := stackURL(p) + "/plans/" + c.Param("planID")
	if r := localPath(c.QueryParam("return")); r != "" {
		to += "?return=" + neturl.QueryEscape(r)
	}
	return respond.Redirect(c, to)
}

// localPath admits a return-to path only if it stays on this site: an absolute
// path that is not scheme-relative (//host). Anything else, full URLs, header
// games, comes back "", and the caller falls back to its own default. The
// param rides a banner link and a hidden form field, so it is user-controlled.
func localPath(p string) string {
	if strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.Contains(p, "\\") {
		return p
	}
	return ""
}

// POST /projects/:id/settings, save this project's cascade overrides.
func (h *handler) SaveSettings(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	vals, err := c.FormParams()
	if err != nil {
		return err
	}
	if err := h.settings.SaveStack(ctx, p, vals); err != nil {
		if !stackrmw.FlashRefusal(c, err) {
			return err
		}
		return respond.Redirect(c, h.settingsSection(ctx, p, "general"))
	}
	middleware.SetFlash(c, "Stack defaults saved.", middleware.FlashSuccess)
	return respond.Redirect(c, h.settingsSection(ctx, p, "general"))
}

// resyncProxy re-renders every app's traefik config after a settings save:
// the cascade feeds the rendered routes (protect), which are
// otherwise only rewritten on domain changes.
func (h *handler) resyncProxy(ctx context.Context) {
	if h.px == nil {
		return
	}
	if err := h.px.Resync(ctx); err != nil {
		slog.Error("proxy resync after settings save", "error", err)
	}
}

// GET /envs/:id/graph/status, node statuses as JSON for the canvas's
// live poll (graph.js patches cards in place; no full re-render).
func (h *handler) GraphStatus(c echo.Context) error {
	ctx := c.Request().Context()
	envID, _, err := h.loadEnvForAnnotation(c)
	if err != nil {
		return err
	}
	g, err := h.buildGraph(ctx, envID, arrangeStyle(c))
	if err != nil {
		return err
	}
	tiles, _ := h.tiles.ListForEnv(ctx, envID)
	return c.JSON(http.StatusOK, map[string]any{
		"nodes":   canvas.StatusNodes(ctx, g.Nodes),
		"traffic": h.envTraffic(tiles),
	})
}

// POST /envs/:id/graph/positions, persist dragged node positions. Accepts one
// node ({node_id,x,y}, sent throttled during a drag) or a batch
// ({nodes:[...]}, sent once on drag end while the canvas still holds
// never-dragged cards). The batch matters: with only the dragged card saved,
// autoLayout treats every unsaved neighbor as a newcomer and re-anchors it
// beside that card (internal/graph/graph.go), which is the first-move jump.
func (h *handler) SaveNodePosition(c echo.Context) error {
	type pos struct {
		NodeID string  `json:"node_id"`
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
	}
	var in struct {
		pos
		Nodes []pos `json:"nodes"`
	}
	if err := c.Bind(&in); err != nil || (in.NodeID == "" && len(in.Nodes) == 0) {
		return echo.NewHTTPError(http.StatusBadRequest, "node_id, x, y required")
	}
	if in.NodeID != "" {
		in.Nodes = append(in.Nodes, in.pos)
	}
	ctx := c.Request().Context()
	env, err := h.envs.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	tiles, err := h.tiles.ListForEnv(ctx, env.ID)
	if err != nil {
		return err
	}
	owner := repo.GraphOwner(repo.ScopeEnv, env.ID)
	ps := make([]repo.NodePosition, 0, len(in.Nodes))
	for _, p := range in.Nodes {
		ps = append(ps, repo.NodePosition{NodeID: slugKey(tiles, p.NodeID), X: p.X, Y: p.Y})
	}
	if err := repo.ValidateNodePositions(owner, ps); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := h.store.SaveNodePositions(ctx, owner, ps); err != nil {
		return err
	}
	h.notifier.Project(env.StackID) // other open canvases re-fetch and move the card
	return c.NoContent(http.StatusNoContent)
}

// POST /envs/:id/graph/positions/reset, drop all saved positions so the
// canvas falls back to the auto-layout.
func (h *handler) ResetNodePositions(c echo.Context) error {
	env, err := h.envs.Get(c.Request().Context(), c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if err := h.store.DeleteNodePositions(c.Request().Context(), repo.GraphOwner(repo.ScopeEnv, env.ID)); err != nil {
		return err
	}
	h.notifier.Project(env.StackID)
	return c.NoContent(http.StatusNoContent)
}

// EnvLogs renders the environment-wide combined log viewer.
// GET /:org/:stack/:env/logs
func (h *handler) EnvLogs(c echo.Context) error {
	p, env, err := h.resolveSlugs(c)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, envLogsPage(c, p, env))
}

// EnvLogsStream merges every tile container's logs into one SSE stream,
// each line prefixed "tile|" so the viewer can colour per tile.
// GET /envs/:id/logs/stream
func (h *handler) EnvLogsStream(c echo.Context) error {
	ctx := c.Request().Context()
	env, err := h.envs.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	tiles, err := h.tiles.ListForEnv(ctx, env.ID)
	if err != nil {
		return err
	}
	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/event-stream")
	res.Header().Set("Cache-Control", "no-cache")
	res.WriteHeader(http.StatusOK)

	merged := make(chan string, 512)
	var wg sync.WaitGroup
	streams := 0
	for i := range tiles {
		t := tiles[i]
		// Nothing that has no long-lived container of its own: a volume tile
		// has no service and never did, and asking for one streams "no such
		// task or service" into the viewer as though it were output. Cron
		// and function runs log per run, not here.
		if t.IsVolume() {
			continue
		}
		if pol, ok := runpolicy.For(t.Kind); ok && !pol.KeepAlive {
			continue
		}
		// The service, when there is one: swarm collects a service's logs
		// from every node through the manager, so this is the only branch
		// that sees a tile running on a worker at all. Going straight to a
		// container by label reads the local socket, which answers for the
		// manager and nowhere else, every off-manager tile was simply
		// missing from this stream, with nothing to say so.
		var (
			ch   <-chan string
			stop func()
			err  error
		)
		if name := envnet.ServiceFor(ctx, h.store, &t); name != "" {
			ch, stop, err = h.rt.StreamServiceLogsMarked(ctx, name, 100)
		}
		if ch == nil {
			label := runtime.LabelApp
			if t.IsManaged() {
				label = runtime.LabelDB
			}
			cs, _ := h.clus.ListByLabel(ctx, label, t.ID)
			if len(cs) == 0 {
				continue
			}
			ch, stop, err = h.clus.StreamLogsMarked(ctx, h.clus.Self(ctx), cs[0].ID, 100)
		}
		if err != nil {
			continue
		}
		defer stop()
		streams++
		wg.Add(1)
		go func(slug string, ch <-chan string) {
			defer wg.Done()
			for line := range ch {
				select {
				case merged <- slug + "|" + line:
				case <-ctx.Done():
					return
				}
			}
		}(t.Slug, ch)
	}
	go func() { wg.Wait(); close(merged) }()
	if streams == 0 {
		_, _ = fmt.Fprint(res, "data: env|O - nothing running in this environment\n\n")
		res.Flush()
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case line, ok := <-merged:
			if !ok {
				return nil
			}
			_, _ = fmt.Fprintf(res, "data: %s\n\n", line)
			res.Flush()
		}
	}
}

// splitTiles separates database-preset tiles from the rest for views that
// still render them as two lists.
func splitTiles(tiles []repo.Tile) (apps, dbs []repo.Tile) {
	for _, t := range tiles {
		if t.IsManaged() {
			dbs = append(dbs, t)
		} else {
			apps = append(apps, t)
		}
	}
	return apps, dbs
}

// slugKey translates a node id between its uuid form ("app:<uuid>", used in
// the DOM) and its slug form ("app:<slug>", used for stack-level persistence
// so every environment shares one layout).
func slugKey(tiles []repo.Tile, nodeID string) string {
	kind, id, ok := strings.Cut(nodeID, ":")
	if !ok {
		return nodeID
	}
	for i := range tiles {
		if tiles[i].ID == id {
			return kind + ":" + tiles[i].Slug
		}
	}
	return nodeID // synthetic nodes (proxy/host) and job ids pass through
}

func uuidKey(tiles []repo.Tile, nodeID string) string {
	kind, slug, ok := strings.Cut(nodeID, ":")
	if !ok {
		return nodeID
	}
	for i := range tiles {
		if tiles[i].Slug == slug {
			return kind + ":" + tiles[i].ID
		}
	}
	return nodeID
}

// arrangeStyle reads the signed-in user's layout engine choice out of their
// graph prefs; anonymous or unset falls back to the default.
func arrangeStyle(c echo.Context) graph.ArrangeStyle {
	prefs := ""
	if u := stackrmw.CurrentUser(c); u != nil {
		prefs = u.GraphPrefs
	}
	return graph.StyleFromPrefs(prefs)
}

func (h *handler) buildGraph(ctx context.Context, envID string, style graph.ArrangeStyle) (graph.Graph, error) {
	tiles, err := h.tiles.ListForEnv(ctx, envID)
	if err != nil {
		return graph.Graph{}, err
	}
	apps, dbs := splitTiles(tiles)
	// Layouts are per-environment: dragging a card in one environment used to
	// rearrange every sibling environment of the same stack.
	rows, err := h.store.ListNodePositions(ctx, repo.GraphOwner(repo.ScopeEnv, envID))
	if err != nil {
		return graph.Graph{}, err
	}
	positions := make(map[string][2]float64, len(rows))
	for _, r := range rows {
		positions[uuidKey(tiles, r.NodeID)] = [2]float64{r.X, r.Y}
	}

	// Latest network sample per tile (cards show live in/out rates).
	rates := graph.NetRates{}
	since := time.Now().Add(-2 * time.Minute)
	for i := range tiles {
		prefix := "app:"
		if tiles[i].IsManaged() {
			prefix = "db:"
		}
		if ms, err := h.store.ListMetrics(ctx, prefix+tiles[i].ID, since); err == nil && len(ms) > 0 {
			last := ms[len(ms)-1]
			rates[tiles[i].ID] = [2]float64{last.RxBps, last.TxBps}
		}
	}
	allDomains, err := h.store.ListDomains(ctx)
	if err != nil {
		return graph.Graph{}, err
	}
	domains := make(map[string][]string)
	for _, d := range allDomains {
		domains[d.TileID] = append(domains[d.TileID], d.Host)
	}
	resources, err := h.store.ListResourcesByEnv(ctx, envID)
	if err != nil {
		return graph.Graph{}, err
	}
	g := graph.Build(apps, dbs, resources, domains, positions, rates, h.envTraffic(tiles), h.tileRefs(ctx, tiles))
	g.AddReferences(h.sharedRefs(ctx, envID, tiles), positions)
	g.AddVarCards(h.envVarCards(ctx, envID, tiles), positions)
	g.Arrange(style, positions)
	markStaged(&g, tiles, h.stagedMarkers(ctx, envID))
	// A cron mid-run: its footer says so until the row closes. The status
	// endpoint re-renders footers on every poll, so it flips back on its own.
	if open, err := h.store.ListOpenCronRuns(ctx); err == nil {
		graph.MarkRunning(&g, open)
	}
	h.markSliceStats(&g)
	// Where each card actually runs. Read from swarm rather than from the
	// rows: task state is the only thing that knows a healthy tile on a
	// worker is healthy, which is the fix for one reading "stopped"
	// (docs/plans/32-multi-node-ui.md, canvas).
	h.markPlacement(ctx, &g, tiles)
	h.addForwards(ctx, &g, tiles)
	g.Annotations, _ = h.store.ListAnnotations(ctx, repo.GraphOwner(repo.ScopeEnv, envID))
	g.Groups, _ = h.store.ListGraphGroups(ctx, repo.GraphOwner(repo.ScopeEnv, envID))
	return g, nil
}

// forwardCounts is the chip-level rollup of addForwards: how many live
// forwards, distinct (tile, port), relay container or open CLI session,
// exist per tile, across all environments.
func (h *handler) forwardCounts(ctx context.Context) map[string]int {
	type key struct {
		tile string
		port int
	}
	seen := map[key]bool{}
	if h.rt != nil { // nil in tests
		rctx, cancel := components.PageCtx(ctx)
		defer cancel()
		if relays, err := h.rt.ListProxyRelays(rctx); err == nil {
			for _, r := range relays {
				seen[key{r.TileID, r.Port}] = true
			}
		}
	}
	if h.forwards != nil {
		for _, s := range h.forwards.Sessions() {
			seen[key{s.TileID, s.Port}] = true
		}
	}
	out := map[string]int{}
	for k := range seen {
		out[k.tile]++
	}
	return out
}

// addForwards appends the ephemeral port-forward cards: one per (tile, port)
// with either a live proxyrelay container (docker) or an open CLI session
// (the registry), the union, because the two outlive each other in both
// directions: a trafficless forward outlasts its relay's 60s idle reap, and a
// relay idles out its last minute after the CLI has gone. Rows name who has a
// forward open.
func (h *handler) addForwards(ctx context.Context, g *graph.Graph, tiles []repo.Tile) {
	if h.rt == nil { // nil in tests
		return
	}
	inEnv := map[string]bool{}
	for i := range tiles {
		inEnv[tiles[i].ID] = true
	}
	type key struct {
		tile string
		port int
	}
	rows := map[key][]graph.ForwardUser{}
	seen := map[key]bool{}
	var order []key

	rctx, cancel := components.PageCtx(ctx)
	defer cancel()
	if relays, err := h.rt.ListProxyRelays(rctx); err == nil {
		for _, r := range relays {
			k := key{r.TileID, r.Port}
			if inEnv[r.TileID] && !seen[k] {
				seen[k] = true
				order = append(order, k)
			}
		}
	}
	if h.forwards != nil {
		sessions := h.forwards.Sessions()
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].StartedAt.Before(sessions[j].StartedAt) })
		for _, s := range sessions {
			k := key{s.TileID, s.Port}
			if !inEnv[s.TileID] {
				continue
			}
			if !seen[k] {
				seen[k] = true
				order = append(order, k)
			}
			name := s.UserName
			if name == "" {
				name = "someone"
			}
			rows[k] = append(rows[k], graph.ForwardUser{
				Name: name, Role: s.Role, Avatar: avatar.URL(s.AvatarPath),
			})
		}
	}

	var fwds []graph.Forward
	for _, k := range order {
		fwds = append(fwds, graph.Forward{TileID: k.tile, Port: k.port, Users: rows[k]})
	}
	sort.Slice(fwds, func(i, j int) bool {
		if fwds[i].TileID != fwds[j].TileID {
			return fwds[i].TileID < fwds[j].TileID
		}
		return fwds[i].Port < fwds[j].Port
	})
	graph.AddForwards(g, fwds)
}

// markSliceStats puts each logical database's own activity on its card. Set
// after Build so the graph package stays free of the sampler.
func (h *handler) markSliceStats(g *graph.Graph) {
	if h.sampler == nil {
		return
	}
	stats := h.sampler.SliceStats()
	if len(stats) == 0 {
		return
	}
	for i := range g.Nodes {
		if g.Nodes[i].Kind != graph.KindResource {
			continue
		}
		if st, ok := stats[strings.TrimPrefix(g.Nodes[i].ID, "resource:")]; ok {
			g.Nodes[i].TxnRate, g.Nodes[i].SizeBytes = st.TxnRate, st.Size
		}
	}
}

// stagedMarkers maps a tile slug to its canvas marker ("delete" for a staged
// teardown, "pending" for any other staged edit). Creates have no committed
// tile/node, so they don't appear here (shown in the pending box + review).
func (h *handler) stagedMarkers(ctx context.Context, envID string) map[string]string {
	changes, err := h.store.ListStagedByEnv(ctx, envID)
	if err != nil {
		return nil
	}
	m := make(map[string]string, len(changes))
	for _, ch := range changes {
		if ch.Summary == "delete" {
			m[ch.TileSlug] = "delete"
		} else if m[ch.TileSlug] == "" {
			m[ch.TileSlug] = "pending"
		}
	}
	return m
}

// markStaged stamps each node with its slug's staging marker (node IDs carry
// the tile id; tiles map id→slug).
func markStaged(g *graph.Graph, tiles []repo.Tile, markers map[string]string) {
	if len(markers) == 0 {
		return
	}
	slugByID := make(map[string]string, len(tiles))
	for i := range tiles {
		slugByID[tiles[i].ID] = tiles[i].Slug
	}
	for i := range g.Nodes {
		id := g.Nodes[i].ID
		if k := strings.IndexByte(id, ':'); k >= 0 {
			id = id[k+1:]
		}
		if mk := markers[slugByID[id]]; mk != "" {
			g.Nodes[i].Staged = mk
		}
	}
}

// sharedRefs finds shared db instances that services in this env provision
// from but which live in another env, those become read-only reference cards
// (same-env instances already render as real nodes). Deterministic order.
// tileRefs maps each tile to the tiles it references, resolved rather than
// guessed from substrings. A managed resource resolves to its provider tile,
// which is what the canvas draws.
//
// Best-effort: a tile whose variables don't resolve (a dangling reference, say)
// contributes no edges instead of blanking the whole canvas. one
// resolve per tile, fine at canvas sizes, batch it if an env ever gets big.
func (h *handler) tileRefs(ctx context.Context, tiles []repo.Tile) map[string][]string {
	out := map[string][]string{}
	r := varref.New(h.store)
	for i := range tiles {
		if tiles[i].IsVolume() || tiles[i].IsManaged() {
			continue // storage draws no edges; a db's own vars are literals
		}
		res, err := r.Resolve(ctx, tiles[i].ID, varref.System)
		if err != nil {
			continue
		}
		// Deduplicate: several variables reading the same source are one edge,
		// not N stacked on top of each other. A provisioned slice stays its own
		// target, it is its own node, with its own credentials.
		seen := map[string]bool{}
		for _, d := range res.Deps {
			if seen[d.ID] {
				continue
			}
			seen[d.ID] = true
			out[tiles[i].ID] = append(out[tiles[i].ID], d.ID)
		}
	}
	return out
}

func (h *handler) sharedRefs(ctx context.Context, envID string, tiles []repo.Tile) []graph.Reference {
	byInstance := map[string]*graph.Reference{}
	var order []string
	// Instances this consumer already reaches through a slice card: the chain
	// consumer -> slice -> instance says it, so a second consumer -> instance
	// edge would draw the same dependency twice.
	viaSlice := map[string]bool{} // "<consumer tile>|<instance tile>"
	if resources, err := h.store.ListResourcesByEnv(ctx, envID); err == nil {
		provider := make(map[string]string, len(resources))
		for _, r := range resources {
			provider[r.ID] = r.ProviderTileID
		}
		for i := range tiles {
			binds, err := h.store.BindingsForConsumer(ctx, tiles[i].ID)
			if err != nil {
				continue
			}
			for _, b := range binds {
				if p, ok := provider[b.ResourceID]; ok {
					viaSlice[tiles[i].ID+"|"+p] = true
				}
			}
		}
	}
	for i := range tiles {
		t := &tiles[i]
		if t.IsManaged() || t.IsVolume() {
			continue
		}
		ps, err := h.store.ListProvisionsByConsumer(ctx, t.ID)
		if err != nil {
			continue
		}
		for _, p := range ps {
			inst, _ := h.tiles.Get(ctx, p.InstanceTileID)
			if inst == nil || inst.EnvironmentID == envID {
				continue // missing, or already a real node on this canvas
			}
			ref := byInstance[inst.ID]
			if ref == nil {
				ref = &graph.Reference{
					InstanceID:   inst.ID,
					Name:         inst.Name,
					Detail:       inst.Engine + " · managed",
					Engine:       inst.Engine,
					Href:         "/dbs/" + inst.ID,
					ExternalPort: inst.ExternalPort,
				}
				if doms, err := h.store.ListDomainsByTile(ctx, inst.ID); err == nil {
					for _, d := range doms {
						ref.Domains = append(ref.Domains, d.Host)
					}
				}
				byInstance[inst.ID] = ref
				order = append(order, inst.ID)
			}
			if viaSlice[t.ID+"|"+inst.ID] {
				continue // the slice card already carries this dependency
			}
			ref.ConsumerIDs = append(ref.ConsumerIDs, t.ID)
		}
	}
	sort.Strings(order)
	out := make([]graph.Reference, 0, len(order))
	for _, id := range order {
		out = append(out, *byInstance[id])
	}
	return out
}

// envNodeOf maps sampler endpoints onto env-canvas cards. Here the cards are
// the containers, so it is the identity, plus the proxy, which the sampler
// calls "proxy" and the canvas gives a node id. Endpoints absent from the map
// are endpoints outside this environment, and RollupTraffic drops them.
func envNodeOf(tiles []repo.Tile) map[string]string {
	nodeOf := map[string]string{"proxy": graph.ProxyNodeID}
	for i := range tiles {
		// Both sampler spellings, each standing for itself: which one a tile
		// answers to is the sampler's business, not the canvas's.
		nodeOf[graph.AppNodeID(tiles[i].ID)] = graph.AppNodeID(tiles[i].ID)
		nodeOf[graph.DBNodeID(tiles[i].ID)] = graph.DBNodeID(tiles[i].ID)
	}
	return nodeOf
}

// envTraffic folds the sampler's flow snapshot onto this env's cards. Shares
// RollupTraffic with the stack and org canvases: its own loop here used to
// skip the rule that drops a flow whose ends land on the same card, so a tile
// talking to itself drew a lane looping back into itself at this level and
// nowhere else.
func (h *handler) envTraffic(tiles []repo.Tile) []graph.TrafficPair {
	if h.sampler == nil {
		return nil
	}
	return graph.RollupTraffic(h.sampler.Snapshot().Pairs, envNodeOf(tiles))
}

// POST /projects/:id/dbs, create a database in this project.
func (h *handler) CreateDB(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	env, err := h.envFromForm(c, p.ID)
	if err != nil {
		return err
	}
	engine := c.FormValue("engine")
	d := &repo.Tile{StackID: p.ID, EnvironmentID: env.ID, Name: c.FormValue("name"), Engine: engine}
	// Scope choice mirrors the file's structure: shared: = stack, in-env =
	// env. Org stays CLI/API-only.
	scope := "env"
	if c.FormValue("scope") == "stack" {
		scope = "stack"
	}
	staged, err := h.instances.Create(ctx, d, scope, stackconf.TileConf{
		Type: "managed", Engine: engine, Scope: scope,
	}, stackrmw.WebActor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	h.fillOrg(ctx, p)
	if staged {
		middleware.SetFlash(c, "Database creation staged. Review & apply on the canvas to deploy.", middleware.FlashSuccess)
		return respond.Redirect(c, envURL(p, env))
	}
	if c.FormValue("from") == "canvas" {
		return respond.Redirect(c, envURL(p, env))
	}
	return respond.Redirect(c, "/dbs/"+d.ID)
}

// atoiOr parses a port-ish int, falling back on empty/invalid input.
func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 65535 {
		return def
	}
	return n
}

// Settings renders the stack settings page (environments, defaults, PR envs).
// GET /:org/:stack/settings
// settingsStack loads the stack named in the URL and checks access. Every
// settings section starts here.
func (h *handler) settingsStack(c echo.Context) (*repo.Stack, error) {
	ctx := c.Request().Context()
	org, err := h.store.GetOrgBySlug(ctx, c.Param("org"))
	if err != nil || org == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "org not found")
	}
	p, err := h.stacks.BySlug(ctx, org.ID, c.Param("stack"))
	if err != nil {
		return nil, stackrmw.HTTP(err)
	}
	p.OrgSlug = org.Slug
	return p, nil
}

// Settings sends /:org/:stack/settings to its first section.
func (h *handler) Settings(c echo.Context) error {
	p, err := h.settingsStack(c)
	if err != nil {
		return err
	}
	return respond.Redirect(c, stackURL(p)+"/settings/general")
}

// GET /:org/:stack/settings/general
func (h *handler) SettingsGeneral(c echo.Context) error {
	p, err := h.settingsStack(c)
	if err != nil {
		return err
	}
	// Orgs this stack could move to, the mover's own orgs (all of them for an
	// admin), minus its current one. Owner-only; empty (the common single-org
	// case) hides the control entirely.
	var moveTargets []repo.Org
	if stackrmw.IsOwner(c) {
		for _, o := range stackrmw.Orgs(c) {
			if o.ID != p.OrgID {
				moveTargets = append(moveTargets, o)
			}
		}
	}
	// Resolved without the stack's own level, so the hints say what the stack
	// would inherit if its fields were cleared. The org sits between the
	// server and the stack, so it is part of that.
	ctx := c.Request().Context()
	res := settings.ForOrg(ctx, h.store, p.OrgID)
	levels, err := settings.Levels(ctx, h.store, "", p.ID, "")
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, stackGeneralPage(c, p, res, levels, moveTargets))
}

// GET /:org/:stack/settings/config
func (h *handler) SettingsConfig(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.settingsStack(c)
	if err != nil {
		return err
	}
	ghConns, err := h.githubConnectors(ctx, p.OrgID)
	if err != nil {
		return err
	}
	// The plans list moved to the releases page: reviewing a plan is not a
	// setting, and it belongs next to the commits it comes from.
	return respond.HTML(c, http.StatusOK, stackConfigPage(c, p, ghConns))
}

// githubConnectors lists the org's connectors that finished their setup.
func (h *handler) githubConnectors(ctx context.Context, orgID string) ([]repo.Connector, error) {
	conns, err := h.store.ListConnectorsByOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	var out []repo.Connector
	for _, cn := range conns {
		if cn.Provider == "github" && githubapp.ParseConfig(cn.Config).Connected() {
			out = append(out, cn)
		}
	}
	return out, nil
}

// GET /:org/:stack/settings/variables, the page, or just the editor when an
// in-page action swapped it.
func (h *handler) SettingsVariables(c echo.Context) error {
	p, err := h.settingsStack(c)
	if err != nil {
		return err
	}
	return h.renderStackVars(c, p)
}

// StackVarsPanel is the canvas drawer view of the stack's variables editor,
// the vars/secrets cards open this instead of leaving the graph.
// GET /:org/:stack/settings/variables/panel
func (h *handler) StackVarsPanel(c echo.Context) error {
	p, err := h.settingsStack(c)
	if err != nil {
		return err
	}
	return h.renderStackVars(c, p)
}

// renderStackVars serves all three surfaces from one path: the canvas drawer,
// an in-page editor swap, and the full settings page. The request's htmx
// target is what picks between them (see varsSurface), so an editor action
// re-renders exactly the surface it was made on.
func (h *handler) renderStackVars(c echo.Context, p *repo.Stack) error {
	ctx := c.Request().Context()
	vars, err := h.vars.List(ctx, service.StackVars(p.ID))
	if err != nil {
		return err
	}
	canWrite := stackrmw.CanWriteOrg(c, h.store, p.OrgID)
	if !canWrite {
		blankSecrets(vars)
	}
	class, editName, editValue := panelParams(c, vars)
	stackrmw.AuditPanelViews(c, h.store, vars, repo.OwnerStack, p.ID)
	surface := varsSurface(c)
	cfg := components.VarsEditCfg{
		PostURL:    "/projects/" + p.ID + "/vars",
		RefScope:   "stack",
		PanelURL:   h.settingsSection(ctx, p, "variables"),
		ValueURL:   stackURL(p) + "/settings/variables/value",
		Class:      class,
		EditName:   editName,
		EditValue:  editValue,
		EditSecret: varSecret(vars, editName),
		RevealName: c.QueryParam("reveal"),
		Adding:     c.QueryParam("new") != "",
		CanWrite:   canWrite,
	}
	if surface == surfaceDrawer {
		cfg.PanelURL = stackURL(p) + "/settings/variables/panel"
		return respond.HTML(c, http.StatusOK, stackVarsPanel(c, p, filterVarsClass(vars, class), cfg))
	}
	cp, err := h.store.LatestSettledConfigPlan(ctx, p.ID)
	if err != nil {
		return err
	}
	orgVars, err := h.vars.List(ctx, service.OrgVars(p.OrgID))
	if err != nil {
		return err
	}
	envVars, err := h.envVariables(ctx, p.ID)
	if err != nil {
		return err
	}
	cfg.Unset = unsetSecrets(cp, vars, orgVars, envVars)
	if surface == surfaceEditor {
		return respond.HTML(c, http.StatusOK, components.VarsEditor(c, vars, cfg))
	}
	links, err := h.store.ListSecretLinks(ctx, repo.OwnerStack, p.ID)
	if err != nil {
		return err
	}
	events, err := h.store.ListAuditEvents(ctx, repo.OwnerStack, p.ID, auditPageSize)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, stackVariablesPage(c, p, vars, links, cfg, events))
}

// StackVarValue hands one variable's plaintext to the drawer's Copy button and
// is the audit hook for it, secrets never sit in the list markup.
// GET /:org/:stack/settings/variables/value?name=X
func (h *handler) StackVarValue(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.settingsStack(c)
	if err != nil {
		return err
	}
	vars, err := h.vars.List(ctx, service.StackVars(p.ID))
	if err != nil {
		return err
	}
	return stackrmw.AuditServeValue(c, h.store, vars, repo.OwnerStack, p.ID, audit.Copy)
}

// auditPageSize bounds the secret-activity list on a settings page.
// newest N, no paging, add paging when someone asks to scroll back.
const auditPageSize = 50

// The three surfaces the variables editor renders on. Which one a request
// wants is the htmx target it asked to swap: the canvas drawer, the editor
// block on a settings page, or neither, meaning a plain page load.
const (
	surfaceDrawer = "drawer"
	surfaceEditor = "editor"
	surfacePage   = "page"
)

func varsSurface(c echo.Context) string {
	switch htmx.GetTarget(c.Request()) {
	case "drawer-body":
		return surfaceDrawer
	case "vars-editor":
		return surfaceEditor
	}
	return surfacePage
}

// blankSecrets is the read-only view: names and the fact of a secret, no
// values. Writers keep the values, they can already mint a share link.
func blankSecrets(vars []repo.Variable) {
	for i := range vars {
		if vars[i].Secret {
			vars[i].Value = ""
		}
	}
}

// envVariables is every environment of the stack and the variables set on it,
// keyed by environment id. An environment with none still gets an entry: it is
// exactly the one that leaves a stack-declared secret unset.
func (h *handler) envVariables(ctx context.Context, stackID string) (map[string][]repo.Variable, error) {
	envs, err := h.envs.ListForStack(ctx, stackID)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]repo.Variable, len(envs))
	for _, e := range envs {
		vars, err := h.vars.List(ctx, service.EnvVars(e.ID))
		if err != nil {
			return nil, err
		}
		out[e.ID] = vars
	}
	return out, nil
}

// unsetSecrets is the stack-scope values the last settled plan found declared
// with no value and nobody has supplied. A stack with no config file has no
// plan, and so no rows.
//
// A stack-declared secret may be valued per environment instead: varref
// resolves the consumer's env row before the stack one, so a name set in every
// environment is set, and one missing from any environment is not. byEnv is
// every environment of the stack, including those with no variables at all.
func unsetSecrets(cp *repo.ConfigPlan, stackVars, orgVars []repo.Variable, byEnv map[string][]repo.Variable) []stackconf.Input {
	if cp == nil {
		return nil
	}
	var plan struct {
		Inputs []stackconf.Input `json:"inputs"`
	}
	if json.Unmarshal([]byte(cp.Plan), &plan) != nil {
		return nil
	}
	have := map[string]bool{}
	for _, vars := range [][]repo.Variable{stackVars, orgVars} {
		for _, v := range vars {
			have[v.Name] = true
		}
	}
	// A name counts as covered per env only when every environment has it.
	everyEnv := map[string]int{}
	for _, vars := range byEnv {
		seen := map[string]bool{}
		for _, v := range vars {
			if !seen[v.Name] {
				seen[v.Name] = true
				everyEnv[v.Name]++
			}
		}
	}
	var out []stackconf.Input
	for _, in := range plan.Inputs {
		if in.Scope != "stack" || have[in.Name] {
			continue
		}
		if len(byEnv) > 0 && everyEnv[in.Name] == len(byEnv) {
			continue
		}
		out = append(out, in)
	}
	return out
}

// varSecret reports whether the named row is a secret, so an edit round trip
// keeps the flag it was stored with.
func varSecret(vars []repo.Variable, name string) bool {
	for i := range vars {
		if vars[i].Name == name {
			return vars[i].Secret
		}
	}
	return false
}

// panelParams reads the editor's view state, which card's class (query on
// open, form field on posts) and which row Edit picked, and resolves the
// edit row's value. Secret values are NOT blanked here: the editor is a
// writers' surface and hands them real values for Copy and edit pre-fill,
// which the audit trail records (see components.VarsEdit).
func panelParams(c echo.Context, vars []repo.Variable) (class, editName, editValue string) {
	class = c.QueryParam("class")
	if class == "" {
		class = c.FormValue("class")
	}
	editName = c.QueryParam("edit")
	for i := range vars {
		if vars[i].Name == editName {
			editValue = vars[i].Value
		}
	}
	return class, editName, editValue
}

// filterVarsClass narrows the list to the clicked card's class; an unknown or
// empty class shows everything.
func filterVarsClass(vars []repo.Variable, class string) []repo.Variable {
	if class != "plain" && class != "secret" {
		return vars
	}
	out := vars[:0]
	for _, v := range vars {
		if v.Secret == (class == "secret") {
			out = append(out, v)
		}
	}
	return out
}

// inEditor reports whether a var post came from one of the htmx editor
// surfaces, which re-render in place instead of redirecting away.
func inEditor(c echo.Context) bool {
	return varsSurface(c) != surfacePage
}

// GET /:org/:stack/settings/environments
func (h *handler) SettingsEnvironments(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.settingsStack(c)
	if err != nil {
		return err
	}
	envs, err := h.envs.ListForStack(ctx, p.ID)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, e := range envs {
		if tiles, err := h.tiles.ListForEnv(ctx, e.ID); err == nil {
			counts[e.ID] = len(tiles)
		}
	}
	org, _ := h.store.GetOrg(ctx, p.OrgID)
	return respond.HTML(c, http.StatusOK, stackEnvironmentsPage(c, p, envs, counts, envcolor.Map(envs, org, p.ConfigManaged())))
}

// SaveEnvColor sets one environment's own colour. The file owns it on a
// managed stack, so that is refused the way every structural write is.
// POST /envs/:id/color
func (h *handler) SaveEnvColor(c echo.Context) error {
	ctx := c.Request().Context()
	env, err := h.envs.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	p, err := h.loadStack(c, env.StackID)
	if err != nil {
		return err
	}
	// No managed refusal any more: a colour is not declared in a stack file,
	// so it was never the file's to own. It used to be refused here and
	// accepted over the API, and the next apply overwrote whichever won.
	v, ok := components.EnvColorFromForm(c.FormValue("color"), c.FormValue("custom"))
	if !ok {
		return echo.NewHTTPError(http.StatusBadRequest, "pick a palette colour or a #rrggbb value")
	}
	if err := h.envs.Update(ctx, env, service.EnvPatch{Color: &v}, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Colour saved.", middleware.FlashSuccess)
	return respond.Redirect(c, h.settingsSection(ctx, p, "environments"))
}

// GET /:org/:stack/settings/environments/:env, one environment's own page.
// These settings used to be a <details> row inside the stack page, which is
// why nobody could find them.
func (h *handler) SettingsEnvironment(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.settingsStack(c)
	if err != nil {
		return err
	}
	envs, err := h.envs.ListForStack(ctx, p.ID)
	if err != nil {
		return err
	}
	var env *repo.Environment
	for i := range envs {
		if envs[i].Slug == c.Param("env") {
			env = &envs[i]
		}
	}
	if env == nil {
		return echo.NewHTTPError(http.StatusNotFound, "environment not found")
	}
	return h.renderEnvVars(c, p, env, len(envs) == 1)
}

// EnvVarsPanel is the canvas drawer view of an environment's variables, the
// env canvas's vars/secrets cards open this.
// GET /:org/:stack/settings/environments/:env/variables/panel
func (h *handler) EnvVarsPanel(c echo.Context) error {
	p, env, err := h.settingsEnv(c)
	if err != nil {
		return err
	}
	return h.renderEnvVars(c, p, env, false)
}

// EnvVarValue is the env drawer's Copy endpoint, plaintext plus audit row.
// GET /:org/:stack/settings/environments/:env/variables/value?name=X
func (h *handler) EnvVarValue(c echo.Context) error {
	_, env, err := h.settingsEnv(c)
	if err != nil {
		return err
	}
	vars, err := h.vars.List(c.Request().Context(), service.EnvVars(env.ID))
	if err != nil {
		return err
	}
	return stackrmw.AuditServeValue(c, h.store, vars, repo.OwnerEnv, env.ID, audit.Copy)
}

// settingsEnv resolves /:org/:stack/settings/environments/:env to its rows.
func (h *handler) settingsEnv(c echo.Context) (*repo.Stack, *repo.Environment, error) {
	p, err := h.settingsStack(c)
	if err != nil {
		return nil, nil, err
	}
	env, err := h.envs.BySlug(c.Request().Context(), p.ID, c.Param("env"))
	if err != nil {
		return nil, nil, stackrmw.HTTP(err)
	}
	return p, env, nil
}

// renderEnvVars serves the environment's variables on all three surfaces, the
// same way renderStackVars does for the stack.
func (h *handler) renderEnvVars(c echo.Context, p *repo.Stack, env *repo.Environment, only bool) error {
	ctx := c.Request().Context()
	vars, err := h.vars.List(ctx, service.EnvVars(env.ID))
	if err != nil {
		return err
	}
	canWrite := stackrmw.CanWriteOrg(c, h.store, p.OrgID)
	if !canWrite {
		blankSecrets(vars)
	}
	class, editName, editValue := panelParams(c, vars)
	stackrmw.AuditPanelViews(c, h.store, vars, repo.OwnerEnv, env.ID)
	base := h.envSettingsURL(ctx, p, env.Slug)
	cfg := components.VarsEditCfg{
		PostURL:  "/envs/" + env.ID + "/vars",
		RefScope: "stack", // an env row shadows the stack-wide value; same reference

		PanelURL:   base,
		ValueURL:   base + "/variables/value",
		Class:      class,
		EditName:   editName,
		EditValue:  editValue,
		EditSecret: varSecret(vars, editName),
		RevealName: c.QueryParam("reveal"),
		Adding:     c.QueryParam("new") != "",
		CanWrite:   canWrite,
	}
	switch varsSurface(c) {
	case surfaceDrawer:
		cfg.PanelURL = base + "/variables/panel"
		return respond.HTML(c, http.StatusOK, envVarsPanel(c, p, env, filterVarsClass(vars, class), cfg))
	case surfaceEditor:
		return respond.HTML(c, http.StatusOK, components.VarsEditor(c, vars, cfg))
	}
	tiles, _ := h.tiles.ListForEnv(ctx, env.ID)
	envs, err := h.envs.ListForStack(ctx, p.ID)
	if err != nil {
		return err
	}
	events, err := h.store.ListAuditEvents(ctx, repo.OwnerEnv, env.ID, auditPageSize)
	if err != nil {
		return err
	}
	// Resolved without the environment's own level: what it inherits.
	res := settings.ForStack(ctx, h.store, p.ID)
	levels, err := settings.Levels(ctx, h.store, "", "", env.ID)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, envSettingsPage(c, p, env, res, levels, len(tiles), only || len(envs) == 1, vars, cfg, events))
}

// GET /:org/:stack/settings/pr
func (h *handler) SettingsPREnv(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.settingsStack(c)
	if err != nil {
		return err
	}
	envs, err := h.envs.ListForStack(ctx, p.ID)
	if err != nil {
		return err
	}
	ghConns, err := h.githubConnectors(ctx, p.OrgID)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, stackPREnvPage(c, p, envs, repo.LoadPRConfig(ctx, h.store, p.ID), ghConns))
}

// SaveStackVar upserts one stack-scoped variable. Stack values are shared by
// every environment and live outside the repo config, so a config-managed stack
// doesn't lock them.
// POST /projects/:id/vars
func (h *handler) SaveStackVar(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	name := strings.TrimSpace(c.FormValue("name"))
	if err := h.vars.Set(ctx, service.StackVars(p.ID), []service.VarWrite{{
		Name: name, Value: components.VarValue(c), Secret: c.FormValue("secret") != "",
	}}, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	if inEditor(c) {
		return h.renderStackVars(c, p)
	}
	middleware.SetFlash(c, name+" saved. Tiles pick it up on their next deploy/run.", middleware.FlashSuccess)
	return respond.Redirect(c, h.settingsSection(ctx, p, "variables"))
}

// DeleteStackVar removes one stack-scoped variable.
// POST /projects/:id/vars/delete
func (h *handler) DeleteStackVar(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := h.vars.Unset(ctx, service.StackVars(p.ID),
		[]string{c.FormValue("name")}, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	if inEditor(c) {
		return h.renderStackVars(c, p)
	}
	middleware.SetFlash(c, "Variable deleted.", middleware.FlashSuccess)
	return respond.Redirect(c, h.settingsSection(ctx, p, "variables"))
}

// varNameOK is the panel's spelling of the one variable-name rule, which now
// lives in the variable service. Kept as a local check only where the form
// refuses before it has anything to hand the service (a plan input names the
// declared secret it fills in).
func varNameOK(name string) bool { return secretNameRe.MatchString(name) }

var secretNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// MintStackLink creates a one-time link for exchanging stack variables with
// someone outside the panel, a client pasting their Stripe keys in, or a
// contractor being shown a connection string once.
// POST /projects/:id/links
func (h *handler) MintStackLink(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}

	// Minting is a write-level act even for a share link, arguably especially
	// then, since the variables page blanks secret values and a share link
	// hands them over in the clear. Read access to the stack is not enough.

	kind := repo.LinkDrop
	if c.FormValue("kind") == repo.LinkShare {
		kind = repo.LinkShare
	}
	var fields []repo.SecretLinkField
	// Two boxes rather than one box and a flag: a drop box usually mixes the
	// two, an API key that must be encrypted next to an account id that
	// needn't be, and asking per line is worse than asking twice.
	for _, spec := range []struct {
		body   string
		secret bool
	}{{c.FormValue("secret_fields"), true}, {c.FormValue("plain_fields"), false}} {
		for _, line := range strings.Split(spec.body, "\n") {
			name, hint, _ := strings.Cut(strings.TrimSpace(line), " ")
			if name == "" {
				continue
			}
			if !varNameOK(name) {
				return echo.NewHTTPError(http.StatusBadRequest, name+": letters, digits, _ . - only")
			}
			fields = append(fields, repo.SecretLinkField{
				Name: name, Secret: spec.secret, Hint: strings.TrimSpace(hint)})
		}
	}
	if len(fields) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "name at least one variable")
	}
	encoded, err := sharelink.EncodeFields(fields)
	if err != nil {
		return err
	}

	hours, err := strconv.Atoi(c.FormValue("ttl_hours"))
	if err != nil || hours < 1 || hours > 24*30 {
		return echo.NewHTTPError(http.StatusBadRequest, "expiry: 1 to 720 hours")
	}
	window, _ := strconv.Atoi(c.FormValue("window_minutes"))
	if window < 0 || window > 60 {
		return echo.NewHTTPError(http.StatusBadRequest, "reveal window: 0 to 60 minutes")
	}

	l := &repo.SecretLink{
		Kind:          kind,
		OwnerKind:     repo.OwnerStack,
		OwnerID:       p.ID,
		Label:         strings.TrimSpace(c.FormValue("label")),
		Fields:        encoded,
		WindowMinutes: window,
		ExpiresAt:     time.Now().Add(time.Duration(hours) * time.Hour),
		CreatedBy:     middleware.GetSubjectID(c),
	}
	token, err := sharelink.Mint(ctx, h.store, l, c.FormValue("passphrase"))
	if err != nil {
		return err
	}

	// The token exists in plaintext only right here. Same deal as an API key:
	// show it once, and a lost link is re-minted rather than recovered.
	url := c.Scheme() + "://" + c.Request().Host + "/s/" + token
	middleware.SetFlash(c, "Link created. Copy it now, it is not shown again: "+url, middleware.FlashSuccess)
	return respond.Redirect(c, h.settingsSection(ctx, p, "variables"))
}

// POST /projects/:id/links/revoke
func (h *handler) RevokeStackLink(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	// Scope check: a link id from another stack must not be revocable here.
	links, err := h.store.ListSecretLinks(ctx, repo.OwnerStack, p.ID)
	if err != nil {
		return err
	}
	id := c.FormValue("id")
	for _, l := range links {
		if l.ID == id {
			if err := sharelink.Revoke(ctx, h.store, id); err != nil {
				return err
			}
			middleware.SetFlash(c, "Link revoked.", middleware.FlashSuccess)
			return respond.Redirect(c, h.settingsSection(ctx, p, "variables"))
		}
	}
	return echo.NewHTTPError(http.StatusNotFound, "link not found")
}

// GET /:org/:stack/settings/domains
func (h *handler) SettingsDomains(c echo.Context) error {
	p, err := h.settingsStack(c)
	if err != nil {
		return err
	}
	all, err := h.store.ListDomainResources(c.Request().Context())
	if err != nil {
		return err
	}
	// Everything the stack can claim under, not just its own rows: a stack
	// with no domains of its own still generates hostnames under its org's.
	res := service.VisibleDomainResources(all, p.ID, p.OrgID)
	return respond.HTML(c, http.StatusOK, stackDomainsPage(c, p, res))
}

// SaveStackDomain adds a stack-level domain resource.
// POST /projects/:id/domain-resources
//
// A domain resource is not a tile, so there is no staged patch that can carry
// it: on a config-managed stack it is refused in both ui_edits modes, which
// the service answers from the stack row.
func (h *handler) SaveStackDomain(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	host := c.FormValue("host")
	if _, err := h.resources.Create(ctx, "stack", p.ID, host, service.ResourceOpts{
		IncludeEnvOnDefault: c.FormValue("include_env_on_default") != "",
		ACMEEmail:           c.FormValue("acme_email"),
	}); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Domain "+host+" added. This stack's tiles can now claim auto hostnames under it.", middleware.FlashSuccess)
	return respond.Redirect(c, h.settingsSection(ctx, p, "domains"))
}

// DeleteStackDomain removes one of this stack's own domain resources.
// POST /projects/:id/domain-resources/delete
func (h *handler) DeleteStackDomain(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	r, err := h.resources.Get(ctx, c.FormValue("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	// This page owns this stack's own rows and nothing else; the id comes
	// from a form field.
	if r.Level != "stack" || r.OwnerID != p.ID {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err := h.resources.Delete(ctx, r.ID); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Domain resource removed. Existing generated hostnames keep working until their tile redeploys.", middleware.FlashSuccess)
	return respond.Redirect(c, h.settingsSection(ctx, p, "domains"))
}

// SaveEnvSettings stores one environment's cascade overrides.
// POST /envs/:id/settings
func (h *handler) SaveEnvSettings(c echo.Context) error {
	ctx := c.Request().Context()
	env, err := h.envs.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	p, err := h.loadStack(c, env.StackID)
	if err != nil {
		return err
	}
	vals, err := c.FormParams()
	if err != nil {
		return err
	}
	if err := h.settings.SaveEnv(ctx, env, vals); err != nil {
		if !stackrmw.FlashRefusal(c, err) {
			return err
		}
		return respond.Redirect(c, h.envSettingsURL(ctx, p, env.Slug))
	}
	middleware.SetFlash(c, "Environment overrides saved.", middleware.FlashSuccess)
	return respond.Redirect(c, h.envSettingsURL(ctx, p, env.Slug))
}

// SaveEnvVar upserts one environment-scoped variable, this environment's own
// value of a stack-declared secret, read by ${{ stack.NAME }} before the stack
// level. Like stack values these live outside the repo config, so a
// config-managed stack doesn't lock them.
// POST /envs/:id/vars
func (h *handler) SaveEnvVar(c echo.Context) error {
	ctx := c.Request().Context()
	env, err := h.envs.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	p, err := h.loadStack(c, env.StackID)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(c.FormValue("name"))
	if err := h.vars.Set(ctx, service.EnvVars(env.ID), []service.VarWrite{{
		Name: name, Value: components.VarValue(c), Secret: c.FormValue("secret") != "",
	}}, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	if inEditor(c) {
		return h.renderEnvVars(c, p, env, false)
	}
	middleware.SetFlash(c, name+" saved for "+env.Name+". Tiles pick it up on their next deploy/run.", middleware.FlashSuccess)
	return respond.Redirect(c, h.envSettingsURL(ctx, p, env.Slug))
}

// DeleteEnvVar removes one environment-scoped variable.
// POST /envs/:id/vars/delete
func (h *handler) DeleteEnvVar(c echo.Context) error {
	ctx := c.Request().Context()
	env, err := h.envs.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	p, err := h.loadStack(c, env.StackID)
	if err != nil {
		return err
	}
	if err := h.vars.Unset(ctx, service.EnvVars(env.ID),
		[]string{c.FormValue("name")}, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	if inEditor(c) {
		return h.renderEnvVars(c, p, env, false)
	}
	middleware.SetFlash(c, "Variable deleted.", middleware.FlashSuccess)
	return respond.Redirect(c, h.envSettingsURL(ctx, p, env.Slug))
}

// ops bundles env lifecycle deps for the shared envops package.
func (h *handler) ops() envops.Ops {
	// DBs is what lets Teardown reclaim an ephemeral env's provisioned slices.
	return envops.Ops{Store: h.store, RT: h.rt, Cluster: h.clus, PX: h.px, DBs: managedtiles.NewService(h.clus, h.store),
		Tiles: h.tiles, Sched: h.sched, Domains: h.domains, Resources: h.resources}
}

// SavePREnv stores the stack's PR-environment webhook config.
// POST /projects/:id/prenv
func (h *handler) SavePREnv(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	// comment: and status: are keys the config file writes — the PR-open hook
	// copies its choice onto the stored config, because that is what the
	// deploy feedback reads — so editing them goes through the gate. Neither
	// surface had it, and the edit silently reverted at the next pull request.
	enabled := c.FormValue("enabled") != ""
	comment := c.FormValue("comment") != ""
	status := c.FormValue("status") != ""
	if _, err := h.prenvs.Update(ctx, p, service.PREnvPatch{
		Enabled: &enabled, Comment: &comment, Status: &status,
	}, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "PR environment settings saved.", middleware.FlashSuccess)
	return respond.Redirect(c, h.settingsSection(ctx, p, "pr"))
}

// RotatePRSecret regenerates the PR webhook secret; the old one stops
// validating immediately (update the GitHub webhook after rotating).
// POST /projects/:id/prenv/rotate
func (h *handler) RotatePRSecret(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	if _, err := h.prenvs.Update(ctx, p, service.PREnvPatch{RotateSecret: true}, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Webhook secret regenerated. Update it in GitHub.", middleware.FlashSuccess)
	return respond.Redirect(c, h.settingsSection(ctx, p, "pr"))
}

// WithDomainResources gives the page the domain-resource service.
func (h *handler) WithDomainResources(r *service.DomainResourceService) *handler {
	h.resources = r
	return h
}
