// Package web is the HTML surface: the full route table, the middleware groups
// behind it, and the shared error rendering. Every page's handler and templates
// live under web/handler in a package that mirrors its URL path, and the
// components they share live under web/components.
package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/hamr/pkg/auth"
	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/FyrmForge/hamr/pkg/storage"
	"github.com/FyrmForge/hamr/pkg/websocket"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/config/orgconf"
	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/stream"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/avatar"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/about"
	accountpage "github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/account"
	apppage "github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/app"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/auth/invite"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/auth/login"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/auth/register"
	backupspage "github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/backups"
	clipage "github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/cli"
	containerpage "github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/container"
	dbpage "github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/db"
	deploymentpage "github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/deployment"
	notificationpage "github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/notification"
	orgpage "github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/org"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/prhook"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/project"
	searchpage "github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/search"
	serverpage "github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/server"
	settingspage "github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/handler/sharepub"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/forward"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/githubapp"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/metrics"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/nodes"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	stackruntime "github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/volmove"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	svcmail "github.com/FyrmForge/stackr/internal/stackrd/service/mail"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	svcproxy "github.com/FyrmForge/stackr/internal/stackrd/service/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Deps holds the dependencies for route registration.
type Deps struct {
	Store         repo.Store
	BaseURL       string
	StaticBaseURL string
	DevMode       bool
	// CookieSecure marks session, flash and CSRF cookies Secure. It follows
	// whether the install serves TLS, not DevMode: a Secure cookie is never
	// sent back over plain HTTP, so a LAN install with STACKR_TLS=off and this
	// left on cannot hold a session at all.
	CookieSecure   bool
	SessionManager *auth.SessionManager
	AuthService    *service.AuthService
	FileStorage    storage.FileStorage
	Hub            *websocket.Hub
	Notifier       *notify.Notifier
	StreamHub      *stream.Hub
	Engine         *deploy.Engine
	Runtime        *stackruntime.Runtime
	Proxy          *svcproxy.Service
	Databases      *managedtiles.Service
	Jobs           *jobs.Service
	Backups        *backup.Service
	// Scheduler re-registers the cron and backup tables after a write.
	Scheduler *scheduler.Service
	// Lifecycle owns stop/restart/pause/run for a tile, shared with the API
	// router so a script and a person get the same side effects.
	Lifecycle *service.TileLifecycleService
	// Tiles owns the tile row: create, update, rename, delete.
	Tiles *service.TileService
	// Telemetry resolves where a tile's logs and metrics come from.
	Telemetry *service.TileTelemetryService
	// Domains owns the hostnames a tile answers on; Resources owns the
	// hostnames stackr may generate names under.
	Domains       *service.DomainService
	Resources     *service.DomainResourceService
	Variables     *service.VariableService
	Environments  *service.EnvironmentService
	Stacks        *service.StackService
	Deploys       *service.DeployService
	Releases      *service.ReleaseService
	Plans         *service.PlanService
	Gate          *service.GateService
	Schedules     *service.BackupScheduleService
	Destinations  *service.BackupDestinationService
	Storage       *service.StorageService
	Settings      *service.SettingsService
	NodeService   *service.NodeService
	Containers    *service.ContainerService
	ImageWatch    *service.ImageWatchService
	Access        *service.AccessService
	Registries    *service.RegistryService
	PREnvs        *service.PREnvService
	Members       *service.MemberService
	Orgs          *service.OrgService
	Audit         *service.AuditService
	Graph         *service.GraphService
	Connectors    *service.ConnectorService
	Notifications *service.NotificationService
	Instances     *service.ManagedInstanceService
	Slices        *service.SliceService
	Metrics       *metrics.Sampler
	Forwards      *forward.Registry
	GitHub        *githubapp.Client
	// Mail is nil when no provider is configured: invites then fall back to
	// copy-the-link, which is the only channel a self-hosted box always has.
	Mail *svcmail.Service
	// Applier drives config-as-code, shared with the API router.
	Applier stackconf.Applier
	// Work is the durable job runner; applies are enqueued, not run inline.
	Work    *workqueue.Queue
	DataDir string
	// RegistrySigner signs the managed registry's pull/push tokens. Nil means
	// the token route answers 503.
	RegistrySigner *registry.Signer
	RegistryPort   string
	// ACMEEmail is the Let's Encrypt account address. Environment-only, but
	// shown read-only next to the TLS settings it belongs with.
	ACMEEmail string
	// OrgConfig drives org-level config-as-code (§6).
	OrgConfig *orgconf.Runner

	// Nodes keeps the servers table in step with the swarm and issues join
	// keys. Cluster is every docker call on any node. Mover runs volume
	// moves (docs/plans/31-node-agent-open-questions.md, 35-cluster.md).
	Nodes   *nodes.Service
	Cluster *cluster.Cluster
	Mover   *volmove.Service
	// Version is the panel's build, shown against each node's agent version.
	Version string
	// Admin runs installation-wide operations, today the panel upgrade.
	Admin *service.AdminService
	// Revoke closes share links a live re-check cannot reach, today when an
	// admin deactivates the account that minted them.
	Revoke *service.RevokeService
}

// RegisterRoutes registers all web route handlers on the server.
func RegisterRoutes(srv *server.Server, deps *Deps) {
	e := srv.Echo()

	// WebSocket endpoint.
	e.GET("/ws", deps.Hub.Handler())

	// Favicon: no icon yet, 204 so browsers stop logging a failed request.
	e.GET("/favicon.ico", func(c echo.Context) error { return c.NoContent(http.StatusNoContent) })

	// The managed registry's token realm. Outside the site group on purpose:
	// docker carries no session and no CSRF token, and a redirect to /login
	// comes back to it as a parse error. A signer failure leaves the route
	// answering 503 rather than taking the panel down with it.
	// The signer is loaded once in main and handed in; loading it here as well
	// raced the registry service on a fresh data dir, and in tests it wrote a
	// private key into the source tree under an empty DataDir. A nil signer
	// registers the 503 route.
	e.GET(registry.TokenPath, registryToken(deps.Store, deps.RegistrySigner))

	// Content Security Policy.
	csp := "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'"
	csp += "; connect-src 'self' ws: wss:"
	// GitHub avatars on the commit log (plan 24).
	csp += "; img-src 'self' data: https://avatars.githubusercontent.com"

	// Site routes.
	site := e.Group("")
	site.Use(middleware.Logging())
	site.Use(hamrmw.ErrorPages(components.ErrorPage))
	site.Use(htmxErrors())
	site.Use(setupPending(deps.Store))
	site.Use(hamrmw.SecureWithConfig(hamrmw.SecureConfig{
		ContentSecurityPolicy: csp,
	}))
	site.Use(skipOnPoll(hamrmw.FlashWithConfig(hamrmw.FlashConfig{Secure: deps.CookieSecure})))
	site.Use(hamrmw.CSRFWithConfig(hamrmw.CSRFConfig{Secure: deps.CookieSecure}))

	auth := hamrmw.NewBrowserAuth(deps.SessionManager,
		hamrmw.WithSubjectLoader(func(reqCtx context.Context, id string) (any, error) {
			u, err := deps.Store.GetUserByID(reqCtx, id)
			// Deactivating a user used to close nothing: Active was checked
			// at login and nowhere else, so an open session kept working for
			// as long as the browser held it. A nil subject here is a dead
			// session, which is what deactivation is supposed to mean.
			if err != nil || u == nil || !u.Active {
				return nil, err
			}
			return u, nil
		}),
		hamrmw.WithLoginRedirect("/login"),
		hamrmw.WithHomeRedirect("/"),
		// Without this an htmx action on a page whose session has died swaps the
		// whole login page into the fragment it targeted, the wizard's name
		// step ends up with a login form inside its panel and the URL unchanged.
		hamrmw.WithHXRedirect(),
	)
	site.Use(auth.Load())
	site.Use(middleware.ThemeContext())
	site.Use(middleware.OrgContext(deps.Store))
	site.Use(middleware.ReadOnlyGuard())

	// mutate registers a mutating route: it mounts the verb check AND records
	// the verb on the route, from one call.
	//
	// The two cannot be done separately on purpose. A route that carried the
	// check but declared nothing would be invisible to the route walk, and a
	// route that declared a verb without mounting the check would read as
	// gated and be open — which is the failure point 18 exists to remove, not
	// to relocate. v is the operation, k is what the route addresses, and
	// param names the path parameter carrying k's reference.
	//
	// The level a verb needs is service's (verbLevels in access.go); the
	// captured level each route enforced before is frozen in
	// handlers/web/routelevel_test.go, and the two are asserted equal there.
	mutate := func(g *echo.Group, method, path string, h echo.HandlerFunc,
		v service.Verb, k service.Kind, param string, extra ...echo.MiddlewareFunc) {
		mw := append([]echo.MiddlewareFunc{
			auth.RequireAuth(),
			middleware.Gate(deps.Store, deps.Access, v, k, param),
		}, extra...)
		g.Add(method, path, h, mw...).Name = string(v)
	}

	// read is mutate for a GET, and the same bargain: mounting the check and
	// declaring the verb are one call, so a route cannot read as gated and be
	// open.
	//
	// Reads went through the same gate helpers the writes did, which is why
	// they could not be gated first: for a read-only handler the helper IS
	// the body check, so the level had to be read out by hand rather than
	// taken from the AST walk that captured the writes. What each read route
	// enforced before is frozen in routeread_test.go.
	//
	// Not every read can take one. A page that spans orgs — the home canvas,
	// search, the API's collection endpoints — has no single org to resolve,
	// and its tenancy is a per-row filter inside the handler, not a gate.
	// Those stay as they are and are listed in routeread_test.go.
	read := func(g *echo.Group, path string, h echo.HandlerFunc,
		v service.Verb, k service.Kind, param string, extra ...echo.MiddlewareFunc) {
		mw := append([]echo.MiddlewareFunc{
			auth.RequireAuth(),
			middleware.Gate(deps.Store, deps.Access, v, k, param),
		}, extra...)
		g.Add(http.MethodGet, path, h, mw...).Name = string(v)
	}

	// authLimit blunts scripted credential hammering on the three endpoints
	// that take a credential from an unauthenticated caller. Own store, so it
	// never shares a bucket with the share links.
	//
	// Keyed off X-Forwarded-For rather than RealIP for the same reason as
	// joinClientIP: every request arrives through the bundled traefik, and
	// echo only honours the header when TRUSTED_PROXIES names that proxy,
	// which nobody sets. On RealIP the whole install would share one bucket
	// and the eleventh login in a minute would lock everyone out.
	authLimit := hamrmw.RateLimitWithConfig(hamrmw.RateLimitConfig{
		Store:   hamrmw.NewMemoryStore(hamrmw.WithMaxSize(10000)),
		Rate:    10,
		Window:  time.Minute,
		KeyFunc: func(c echo.Context) (string, error) { return clientIP(c), nil },
	})

	// adminOnly gates host-level pages (servers, containers, global settings).
	adminOnly := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if !middleware.IsAdmin(c) {
				return echo.NewHTTPError(http.StatusNotFound, "not found")
			}
			return next(c)
		}
	}

	settingsHandler := settingspage.NewHandler(deps.Store, deps.Backups, deps.Admin, deps.Runtime, deps.Proxy, deps.GitHub, deps.RegistrySigner, deps.DataDir, deps.RegistryPort, deps.ACMEEmail, deps.BaseURL).
		WithScheduler(deps.Scheduler).
		WithDestinations(deps.Destinations).
		WithRegistries(deps.Registries).
		WithImageWatch(deps.ImageWatch).
		WithRevoke(deps.Revoke).
		WithOrgs(deps.Orgs).
		WithSettings(deps.Settings).WithSchedules(deps.Schedules).WithRegistries(deps.Registries).
		WithAuth(deps.AuthService).WithAudit(deps.Audit).WithConnectors(deps.Connectors)

	searchHandler := searchpage.NewHandler(deps.Store, deps.Environments, deps.Stacks, deps.Tiles).
		WithOrgs(deps.Orgs).WithMembers(deps.Members).WithDomains(deps.Domains).WithPlans(deps.Plans).WithSchedules(deps.Schedules).
		WithConnectors(deps.Connectors).WithInstances(deps.Instances).WithVariables(deps.Variables)
	site.GET("/search", searchHandler.Search, auth.RequireAuth())

	orgHandler := orgpage.NewHandler(deps.Store, deps.Notifier, deps.Metrics, deps.FileStorage, deps.Runtime, deps.Forwards, deps.OrgConfig, deps.GitHub, deps.Mail, deps.RegistrySigner, deps.Proxy).
		WithDomainResources(deps.Resources).WithVariables(deps.Variables).WithStacks(deps.Stacks).
		WithDomains(deps.Domains).WithSlices(deps.Slices).WithStorage(deps.Storage).
		WithGraph(deps.Graph).WithConnectors(deps.Connectors).
		WithPlans(deps.Plans).WithDeploys(deps.Deploys).
		WithAudit(deps.Audit).WithAuth(deps.AuthService).
		WithOrgs(deps.Orgs).WithMembers(deps.Members).
		WithTiles(deps.Tiles).
		WithEnvironments(deps.Environments).
		WithWork(deps.Work).
		WithSettings(deps.Settings)
	// "/" is the root canvas, every org the viewer belongs to, one card each,
	// and each card drills into that org's own canvas. Its saved layout is
	// per-user (repo.ScopeUser), so these three routes carry no :id.
	site.GET("/", orgHandler.Home, auth.RequireAuth())
	site.GET("/graph/status", orgHandler.HomeStatus, auth.RequireAuth())
	site.POST("/graph/positions", orgHandler.SaveHomeNodePosition, auth.RequireAuth())
	site.POST("/graph/positions/reset", orgHandler.ResetHomeNodePositions, auth.RequireAuth())
	site.POST("/graph/annotations", orgHandler.SaveHomeAnnotation, auth.RequireAuth())
	site.POST("/graph/annotations/delete", orgHandler.DeleteHomeAnnotation, auth.RequireAuth())
	site.POST("/graph/groups", orgHandler.SaveHomeGraphGroup, auth.RequireAuth())
	site.POST("/graph/groups/delete", orgHandler.DeleteHomeGraphGroup, auth.RequireAuth())
	// The onboarding wizard. Step 1 has no org yet, so it sits at the top level;
	// the rest hang off the org they are configuring, which is all the state the
	// wizard has.
	//
	// Creating an organization is a server admin's call, so step 1 and the POST
	// it submits to are both adminOnly. Gating only the POST would leave a
	// wizard that dead-ends on its first button. The steps after it hang off an
	// org that already exists and stay owner-gated: an admin can hand an org to
	// an owner who is not an admin, and that owner still has to finish setting
	// it up.
	read(site, "/setup", orgHandler.SetupStart, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/orgs/:slug/setup/:step", orgHandler.Setup, service.VerbOrgOwnerRead, service.KindOrg, "slug")
	// The wizard's own POST routes. Each is served by the handler that owns the
	// same change in settings; arriving here is what makes it redirect back to
	// the step instead of to the settings tab (backTo in handler/org/setup.go).
	//
	// ownerOnly on the two that are served by a handler gated at member level
	// in its settings home. The wizard's other steps are all owner, and the
	// setup allow-list waives the draft gate for this whole prefix, so without
	// it a member who cannot see the wizard could still post its domain and
	// connector steps. AccessService holds the level; this is the route that
	// asks for it (point 15, VerbSetupDomain / VerbSetupConnector).
	setupVerb := func(v service.Verb) echo.MiddlewareFunc {
		return middleware.RequireVerb(deps.Store, deps.Access, v)
	}
	mutate(site, "POST", "/orgs/:slug/setup/name", orgHandler.Rename, service.VerbSetupStep, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/setup/mode", orgHandler.SetupMode, service.VerbSetupStep, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/setup/connector", settingsHandler.GitHubConnect, service.VerbSetupConnector, service.KindOrg, "slug", setupVerb(service.VerbSetupConnector))
	mutate(site, "POST", "/orgs/:slug/setup/config", orgHandler.SaveOrgConfig, service.VerbSetupStep, service.KindOrg, "slug")
	read(site, "/orgs/:slug/setup/config/plan", orgHandler.SetupConfigPlan, service.VerbOrgOwnerRead, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/setup/config/plan/:planID/approve", orgHandler.SetupApprovePlan, service.VerbSetupStep, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/setup/config/plan/:planID/reject", orgHandler.SetupRejectPlan, service.VerbSetupStep, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/setup/config/plan/:planID/inputs", orgHandler.SetPlanInput, service.VerbSetupStep, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/setup/domain", orgHandler.SaveOrgDomain, service.VerbSetupDomain, service.KindOrg, "slug", setupVerb(service.VerbSetupDomain))
	mutate(site, "POST", "/orgs/:slug/setup/team/members", orgHandler.AddMember, service.VerbSetupStep, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/setup/team/members/:userID/role", orgHandler.SetMemberRole, service.VerbSetupStep, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/setup/team/members/:userID/remove", orgHandler.RemoveMember, service.VerbSetupStep, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/setup/team/invites/:inviteID/resend", orgHandler.ResendInvite, service.VerbSetupStep, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/setup/team/invites/:inviteID/reinvite", orgHandler.ReinviteMember, service.VerbSetupStep, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/setup/team/invites/:inviteID/delete", orgHandler.DeleteInvite, service.VerbSetupStep, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/setup/done", orgHandler.SetupDone, service.VerbSetupStep, service.KindOrg, "slug")
	read(site, "/orgs/:slug", orgHandler.Graph, service.VerbOrgRead, service.KindOrg, "slug")
	read(site, "/orgs/:slug/graph/status", orgHandler.GraphStatus, service.VerbOrgRead, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/graph/positions", orgHandler.SaveNodePosition, service.VerbOrgGraphWrite, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/graph/positions/reset", orgHandler.ResetNodePositions, service.VerbOrgGraphWrite, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/graph/annotations", orgHandler.SaveAnnotation, service.VerbOrgGraphWrite, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/graph/annotations/delete", orgHandler.DeleteAnnotation, service.VerbOrgGraphWrite, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/graph/groups", orgHandler.SaveGraphGroup, service.VerbOrgGraphWrite, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/graph/groups/delete", orgHandler.DeleteGraphGroup, service.VerbOrgGraphWrite, service.KindOrg, "slug")
	// The org's settings page: members, invites, connectors, variables, rename
	// and delete. ":id" accepts the org's slug or its uuid.
	read(site, "/orgs/:slug/plans", orgHandler.Plans, service.VerbOrgOwnerRead, service.KindOrg, "slug")
	read(site, "/orgs/:slug/plans/:planID", orgHandler.OrgPlanView, service.VerbOrgOwnerRead, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/plans/:planID/approve", orgHandler.ApproveOrgPlan, service.VerbOrgPlanApprove, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/plans/:planID/reject", orgHandler.RejectOrgPlan, service.VerbOrgPlanApprove, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/plans/:planID/inputs", orgHandler.SetPlanInput, service.VerbOrgPlanApprove, service.KindOrg, "slug")
	// Settings are closed until the org has been through the wizard, so the
	// steps cannot be skipped sideways by clicking into a tab. Everything that
	// has to stay reachable mid-setup (the canvas, the plans pages, the repo
	// picker's lookups) is registered outside this group.
	orgSettings := site.Group("/orgs/:slug/settings", auth.RequireAuth(), middleware.RequireSetupDone(deps.Store))
	read(orgSettings, "", orgHandler.SettingsIndex, service.VerbOrgRead, service.KindOrg, "slug")
	read(orgSettings, "/general", orgHandler.SettingsGeneral, service.VerbOrgRead, service.KindOrg, "slug")
	read(orgSettings, "/members", orgHandler.SettingsMembers, service.VerbMemberList, service.KindOrg, "slug")
	read(orgSettings, "/invites", orgHandler.SettingsInvites, service.VerbOrgOwnerRead, service.KindOrg, "slug")
	read(orgSettings, "/connectors", orgHandler.SettingsConnectors, service.VerbOrgRead, service.KindOrg, "slug")
	read(orgSettings, "/variables", orgHandler.SettingsVariables, service.VerbOrgRead, service.KindOrg, "slug")
	read(orgSettings, "/variables/panel", orgHandler.VarsPanel, service.VerbOrgRead, service.KindOrg, "slug")
	read(orgSettings, "/variables/value", orgHandler.OrgVarValue, service.VerbVariableWrite, service.KindOrg, "slug")
	read(orgSettings, "/domains", orgHandler.SettingsDomains, service.VerbOrgRead, service.KindOrg, "slug")
	// Owner, not write: the delete on the next line was already owner, and a
	// surface disagreeing with itself about a level resolves upward — see
	// 06-points-18-20.md, "The four open decisions", #1.
	mutate(orgSettings, "POST", "/domains", orgHandler.SaveOrgDomain, service.VerbOrgWrite, service.KindOrg, "slug")
	mutate(orgSettings, "POST", "/domains/delete", orgHandler.DeleteOrgDomain, service.VerbOrgWrite, service.KindOrg, "slug")
	read(orgSettings, "/storage", orgHandler.SettingsStorage, service.VerbOrgRead, service.KindOrg, "slug")
	mutate(orgSettings, "POST", "/env-colors", orgHandler.SaveEnvColor, service.VerbOrgWrite, service.KindOrg, "slug")
	read(orgSettings, "/backups", orgHandler.SettingsBackups, service.VerbOrgRead, service.KindOrg, "slug")
	read(orgSettings, "/config/export", orgHandler.ExportConfig, service.VerbConfigExport, service.KindOrg, "slug")
	read(orgSettings, "/defaults", orgHandler.SettingsDefaults, service.VerbOrgRead, service.KindOrg, "slug")
	mutate(orgSettings, "POST", "/defaults", orgHandler.SaveDefaults, service.VerbOrgDefaults, service.KindOrg, "slug")
	read(orgSettings, "/registry", orgHandler.SettingsRegistry, service.VerbOrgRead, service.KindOrg, "slug")
	read(orgSettings, "/registry/images", orgHandler.RegistryImages, service.VerbRegistryList, service.KindOrg, "slug")
	mutate(orgSettings, "POST", "/registry/credentials", orgHandler.CreateRegistryCredential, service.VerbRegistryCredential, service.KindOrg, "slug")
	mutate(orgSettings, "POST", "/registry/credentials/delete", orgHandler.DeleteRegistryCredential, service.VerbRegistryCredential, service.KindOrg, "slug")
	mutate(orgSettings, "POST", "/registry/tags/delete", orgHandler.DeleteRegistryTag, service.VerbRegistryTagDelete, service.KindOrg, "slug")
	read(orgSettings, "/config", orgHandler.SettingsConfig, service.VerbOrgRead, service.KindOrg, "slug")
	mutate(orgSettings, "POST", "/config", orgHandler.SaveOrgConfig, service.VerbOrgConfigBind, service.KindOrg, "slug")
	mutate(orgSettings, "POST", "/backups/destinations", settingsHandler.CreateDestination, service.VerbBackupWrite, service.KindOrg, "slug")
	mutate(orgSettings, "POST", "/backups/destinations/:destID/delete", settingsHandler.DeleteDestination, service.VerbBackupWrite, service.KindOrg, "slug")
	mutate(orgSettings, "POST", "/vars", orgHandler.SaveOrgVar, service.VerbVariableWrite, service.KindOrg, "slug")
	mutate(orgSettings, "POST", "/vars/delete", orgHandler.DeleteOrgVar, service.VerbVariableWrite, service.KindOrg, "slug")
	mutate(orgSettings, "POST", "/connectors/github", settingsHandler.GitHubConnect, service.VerbConnectorWrite, service.KindOrg, "slug")
	mutate(orgSettings, "POST", "/connectors/:connectorID/delete", settingsHandler.DeleteConnector, service.VerbConnectorWrite, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/rename", orgHandler.Rename, service.VerbOrgWrite, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/delete", orgHandler.Delete, service.VerbOrgWrite, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/members", orgHandler.AddMember, service.VerbMemberManage, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/members/:userID/role", orgHandler.SetMemberRole, service.VerbMemberManage, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/members/:userID/remove", orgHandler.RemoveMember, service.VerbMemberManage, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/invites", orgHandler.CreateInvite, service.VerbMemberManage, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/invites/:inviteID/delete", orgHandler.DeleteInvite, service.VerbMemberManage, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/invites/:inviteID/resend", orgHandler.ResendInvite, service.VerbMemberManage, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs/:slug/invites/:inviteID/reinvite", orgHandler.ReinviteMember, service.VerbMemberManage, service.KindOrg, "slug")
	mutate(site, "POST", "/orgs", orgHandler.Create, service.VerbOrgCreate, service.KindNone, "", adminOnly)
	site.POST("/orgs/switch", orgHandler.Switch, auth.RequireAuth())
	mutate(site, "POST", "/orgs/stacks/:id/move", orgHandler.MoveStack, service.VerbStackWrite, service.KindStack, "id")

	serverHandler := serverpage.NewHandler(serverpage.Deps{
		Store: deps.Store, Runtime: deps.Runtime, Proxy: deps.Proxy,
		Nodes: deps.Nodes, Cluster: deps.Cluster,
		Mover: deps.Mover, BaseURL: deps.BaseURL,
		Version: deps.Version,
	}).WithDomainResources(deps.Resources).WithStorage(deps.Storage).WithSettings(deps.Settings).WithNodeService(deps.NodeService).WithTiles(deps.Tiles).
		WithTelemetry(deps.Telemetry).WithRegistries(deps.Registries)
	// The join script is the one route here with no session in front of it.
	// It is curled by a machine that has no login and is not in the swarm
	// yet; the one-time key in the URL, bound to that machine's address, is
	// the authentication (docs/plans/32-multi-node-ui.md, Add node).
	e.GET("/join/:key", serverHandler.JoinScript)
	// The other half of the same trust: the joined node reports the swarm node
	// id it was given, so the row is keyed on that rather than on an address
	// the daemon may not advertise. Same key, re-read inside its TTL, so it is
	// outside the session for the same reason.
	e.POST("/join/:key/node", serverHandler.ClaimNode)
	read(site, "/servers", serverHandler.List, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/servers/add", serverHandler.AddNodeForm, service.VerbAdminRead, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/add", serverHandler.AddNode, service.VerbNodeManage, service.KindNone, "", adminOnly)
	read(site, "/servers/move/:id", serverHandler.MoveForm, service.VerbAdminRead, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/move/:id", serverHandler.Move, service.VerbNodeManage, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/join-key", serverHandler.NewJoinKey, service.VerbNodeManage, service.KindNone, "", adminOnly)
	read(site, "/servers/:id/drain", serverHandler.DrainForm, service.VerbAdminRead, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/drain", serverHandler.Drain, service.VerbNodeManage, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/activate", serverHandler.Activate, service.VerbNodeManage, service.KindNone, "", adminOnly)
	read(site, "/servers/:id/remove", serverHandler.RemoveForm, service.VerbAdminRead, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/remove", serverHandler.Remove, service.VerbNodeManage, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/group", serverHandler.SaveGroup, service.VerbNodeManage, service.KindNone, "", adminOnly)
	read(site, "/servers/:id", serverHandler.Detail, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/servers/:id/host", serverHandler.Host, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/servers/:id/volumes", serverHandler.Volumes, service.VerbAdminRead, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/settings", serverHandler.SaveSettings, service.VerbNodeManage, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/volumes", serverHandler.CreateVolume, service.VerbNodeManage, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/volumes/delete", serverHandler.DeleteVolume, service.VerbNodeManage, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/storage", serverHandler.CreateStorage, service.VerbNodeManage, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/storage/delete", serverHandler.DeleteStorage, service.VerbNodeManage, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/storage/probe", serverHandler.ProbeStorage, service.VerbNodeManage, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/storage/paths", serverHandler.CreateStoragePath, service.VerbNodeManage, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/storage/paths/delete", serverHandler.DeleteStoragePath, service.VerbNodeManage, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/domains", serverHandler.CreateDomainResource, service.VerbNodeManage, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/servers/:id/domains/delete", serverHandler.DeleteDomainResource, service.VerbNodeManage, service.KindNone, "", adminOnly)

	applier := deps.Applier
	projectHandler := project.NewHandler(deps.Store, deps.Runtime, deps.Cluster, deps.Proxy, deps.Metrics, deps.Notifier, deps.GitHub, applier, deps.Forwards).
		WithOrgs(deps.Orgs).WithSlices(deps.Slices).WithNodeService(deps.NodeService).
		WithAudit(deps.Audit).WithRevoke(deps.Revoke).WithTelemetry(deps.Telemetry).
		WithConnectors(deps.Connectors).WithLifecycle(deps.Lifecycle).
		WithGraph(deps.Graph).
		WithInstances(deps.Instances).
		WithVariables(deps.Variables).
		WithEnvironments(deps.Environments).
		WithStacks(deps.Stacks).
		WithDeploys(deps.Deploys).
		WithReleases(deps.Releases).
		WithPlans(deps.Plans).
		WithPREnvs(deps.PREnvs).
		WithMover(deps.Mover).
		WithWork(deps.Work).
		WithSettings(deps.Settings).
		WithScheduler(deps.Scheduler).
		WithTiles(deps.Tiles).
		WithDomains(deps.Domains).
		WithDomainResources(deps.Resources)
	mutate(site, "POST", "/projects", projectHandler.Create, service.VerbStackCreate, service.KindDeferred, "")
	// Legacy id URLs redirect to the canonical slug URLs.
	read(site, "/projects/:id", projectHandler.RedirectStack, service.VerbOrgRead, service.KindStack, "id")
	read(site, "/projects/:id/graph", projectHandler.RedirectStack, service.VerbOrgRead, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/delete", projectHandler.Delete, service.VerbStackWrite, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/settings", projectHandler.SaveSettings, service.VerbStackWrite, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/config", projectHandler.SaveConfigBinding, service.VerbStackWrite, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/config/plan", projectHandler.PlanNow, service.VerbStackPlan, service.KindStack, "id")
	// The pre-rename URL, kept alive because links to it exist; the page
	// itself now lives under /:org/:stack.
	read(site, "/projects/:id/config/plans/:planID", projectHandler.PlanRedirect, service.VerbOrgRead, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/config/plans/:planID/approve", projectHandler.ApprovePlan, service.VerbStackPlanApprove, service.KindStackPlan, "planID")
	mutate(site, "POST", "/projects/:id/config/plans/:planID/reject", projectHandler.RejectPlan, service.VerbStackPlanApprove, service.KindStackPlan, "planID")
	mutate(site, "POST", "/projects/:id/config/plans/:planID/inputs", projectHandler.SetPlanInput, service.VerbStackPlanApprove, service.KindStackPlan, "planID")
	mutate(site, "POST", "/envs/:id/config", projectHandler.SaveEnvConfig, service.VerbEnvWrite, service.KindEnv, "id")
	mutate(site, "POST", "/projects/:id/apps", projectHandler.CreateTile, service.VerbStackWrite, service.KindStack, "id")
	read(site, "/projects/:id/repos", projectHandler.Repos, service.VerbOrgRead, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/dbs", projectHandler.CreateDB, service.VerbStackWrite, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/envs", projectHandler.CreateEnvironment, service.VerbStackWrite, service.KindStack, "id")
	mutate(site, "POST", "/envs/:id/delete", projectHandler.DeleteEnvironment, service.VerbEnvWrite, service.KindEnv, "id")
	mutate(site, "POST", "/envs/:id/reset", projectHandler.ResetEnvironment, service.VerbEnvWrite, service.KindEnv, "id")
	mutate(site, "POST", "/envs/:id/settings", projectHandler.SaveEnvSettings, service.VerbEnvWrite, service.KindEnv, "id")
	mutate(site, "POST", "/envs/:id/color", projectHandler.SaveEnvColor, service.VerbEnvWrite, service.KindEnv, "id")
	mutate(site, "POST", "/envs/:id/vars", projectHandler.SaveEnvVar, service.VerbVariableWrite, service.KindEnv, "id")
	mutate(site, "POST", "/envs/:id/vars/delete", projectHandler.DeleteEnvVar, service.VerbVariableWrite, service.KindEnv, "id")
	mutate(site, "POST", "/projects/:id/vars", projectHandler.SaveStackVar, service.VerbVariableWrite, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/vars/delete", projectHandler.DeleteStackVar, service.VerbVariableWrite, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/links", projectHandler.MintStackLink, service.VerbShareLinkMint, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/links/revoke", projectHandler.RevokeStackLink, service.VerbShareLinkMint, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/domain-resources", projectHandler.SaveStackDomain, service.VerbDomainWrite, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/domain-resources/delete", projectHandler.DeleteStackDomain, service.VerbDomainWrite, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/prenv", projectHandler.SavePREnv, service.VerbStackWrite, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/prenv/rotate", projectHandler.RotatePRSecret, service.VerbStackWrite, service.KindStack, "id")
	// Environment-scoped canvas data (graph.js reads these off data attrs).
	read(site, "/envs/:id/graph/status", projectHandler.GraphStatus, service.VerbOrgRead, service.KindEnv, "id")
	read(site, "/projects/:id/staging/:envID", projectHandler.StagingReview, service.VerbOrgRead, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/staging/:envID/apply", projectHandler.StagingApply, service.VerbStackPlanApprove, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/staging/:envID/discard", projectHandler.StagingDiscard, service.VerbStackPlanApprove, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/staging/:envID/changes/:changeID/discard", projectHandler.StagingDiscardOne, service.VerbStackPlanApprove, service.KindStack, "id")
	read(site, "/envs/:id/logs/stream", projectHandler.EnvLogsStream, service.VerbOrgRead, service.KindEnv, "id")
	mutate(site, "POST", "/envs/:id/graph/positions", projectHandler.SaveNodePosition, service.VerbEnvWrite, service.KindEnv, "id")
	mutate(site, "POST", "/envs/:id/graph/positions/reset", projectHandler.ResetNodePositions, service.VerbEnvWrite, service.KindEnv, "id")
	mutate(site, "POST", "/envs/:id/graph/annotations", projectHandler.SaveEnvAnnotation, service.VerbEnvWrite, service.KindEnv, "id")
	mutate(site, "POST", "/envs/:id/graph/annotations/delete", projectHandler.DeleteEnvAnnotation, service.VerbEnvWrite, service.KindEnv, "id")
	mutate(site, "POST", "/envs/:id/graph/groups", projectHandler.SaveEnvGraphGroup, service.VerbEnvWrite, service.KindEnv, "id")
	mutate(site, "POST", "/envs/:id/graph/groups/delete", projectHandler.DeleteEnvGraphGroup, service.VerbEnvWrite, service.KindEnv, "id")
	// The environments pill and panel on the stack canvas (plan 24).
	read(site, "/projects/:id/envs/compare", projectHandler.EnvCompare, service.VerbOrgRead, service.KindStack, "id")
	// Promotion: one dialogue, one endpoint, wherever the button sits (the
	// releases page, a config plan row). The GET renders the confirm as a
	// fragment because it carries the commits going in, not one sentence.
	read(site, "/projects/:id/envs/:slug/promote", projectHandler.PromoteDialogue, service.VerbOrgRead, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/envs/:slug/promote", projectHandler.PromoteCommit, service.VerbStackWrite, service.KindStack, "id")
	mutate(site, "POST", "/envs/:id/intended", projectHandler.MarkIntended, service.VerbEnvWrite, service.KindEnv, "id")
	mutate(site, "POST", "/envs/:id/copy", projectHandler.CopyEnv, service.VerbEnvWrite, service.KindEnv, "id")
	// Stack canvas: same three endpoints one level up (see StackGraph).
	read(site, "/projects/:id/graph/status", projectHandler.StackGraphStatus, service.VerbOrgRead, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/graph/positions", projectHandler.SaveStackNodePosition, service.VerbStackWrite, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/graph/positions/reset", projectHandler.ResetStackNodePositions, service.VerbStackWrite, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/graph/annotations", projectHandler.SaveStackAnnotation, service.VerbStackWrite, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/graph/annotations/delete", projectHandler.DeleteStackAnnotation, service.VerbStackWrite, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/graph/groups", projectHandler.SaveStackGraphGroup, service.VerbStackWrite, service.KindStack, "id")
	mutate(site, "POST", "/projects/:id/graph/groups/delete", projectHandler.DeleteStackGraphGroup, service.VerbStackWrite, service.KindStack, "id")

	dbHandler := dbpage.NewHandler(deps.Store, deps.Databases, deps.Cluster, deps.Proxy, deps.Notifier).
		WithServices(deps.Instances, deps.Slices, deps.Lifecycle, deps.Domains, deps.Telemetry).
		WithTiles(deps.Tiles).WithStacks(deps.Stacks)
	read(site, "/dbs/:id", redirectTile(deps.Store), service.VerbTileRead, service.KindTile, "id")
	read(site, "/dbs/:id/panel", dbHandler.Panel, service.VerbTileRead, service.KindTile, "id")
	read(site, "/dbs/:id/panel/content", dbHandler.PanelContent, service.VerbTileRead, service.KindTile, "id")
	read(site, "/dbs/:id/volume/panel", dbHandler.VolumePanel, service.VerbTileRead, service.KindTile, "id")
	read(site, "/dbs/:id/volume/files", dbHandler.VolumeFiles, service.VerbTileRead, service.KindTile, "id")
	read(site, "/dbs/:id/volume/files/download", dbHandler.VolumeFileDownload, service.VerbTileRead, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/volume/files/upload", dbHandler.VolumeFileUpload, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/volume/files/delete", dbHandler.VolumeFileDelete, service.VerbTileWrite, service.KindTile, "id")
	read(site, "/dbs/:id/buckets/:bucket/panel", dbHandler.BucketPanel, service.VerbTileRead, service.KindTile, "id")
	read(site, "/dbs/:id/buckets/:bucket/files", dbHandler.BucketFiles, service.VerbTileRead, service.KindTile, "id")
	read(site, "/dbs/:id/buckets/:bucket/files/download", dbHandler.BucketFileDownload, service.VerbTileRead, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/buckets/:bucket/files/upload", dbHandler.BucketFileUpload, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/buckets/:bucket/files/delete", dbHandler.BucketFileDelete, service.VerbTileWrite, service.KindTile, "id")
	read(site, "/dbs/:id/data", dbHandler.Data, service.VerbTileRead, service.KindTile, "id")
	read(site, "/dbs/:id/data/cell", dbHandler.DataCell, service.VerbTileRead, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/data/update", dbHandler.DataUpdate, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/data/insert", dbHandler.DataInsert, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/data/delete", dbHandler.DataDelete, service.VerbTileWrite, service.KindTile, "id")
	read(site, "/dbs/:id/pgdbs/:db/panel", dbHandler.PGDBPanel, service.VerbTileRead, service.KindTile, "id")
	read(site, "/dbs/:id/metrics", dbHandler.Metrics, service.VerbTileRead, service.KindTile, "id")
	read(site, "/dbs/:id/logs/stream", dbHandler.LogsStream, service.VerbTileRead, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/deploy", dbHandler.Deploy, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/stop", dbHandler.Stop, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/start", dbHandler.Start, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/delete", dbHandler.Delete, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/port", dbHandler.SetPort, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/scope", dbHandler.SetScope, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/domains", dbHandler.CreateDomain, service.VerbDomainWrite, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/domains/:domainID/delete", dbHandler.DeleteDomain, service.VerbDomainWrite, service.KindTile, "id")
	read(site, "/dbs/:id/provisions", dbHandler.Provisions, service.VerbTileRead, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/provisions/drop", dbHandler.DropProvision, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/provisions/public", dbHandler.SetProvisionPublic, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/dbs/:id/provisions/fork", dbHandler.ForkProvision, service.VerbTileWrite, service.KindTile, "id")

	// The Backups tab, shared by database tiles and volume tiles: one fragment
	// both panel packages fetch, rather than the same markup twice.
	backupsHandler := backupspage.NewHandler(deps.Store, deps.Backups).WithScheduler(deps.Scheduler).WithBackups(deps.Schedules, deps.Destinations).WithTiles(deps.Tiles).
		WithSlices(deps.Slices)
	read(site, "/tiles/:id/backups", backupsHandler.Panel, service.VerbTileRead, service.KindTile, "id")
	mutate(site, "POST", "/tiles/:id/backups", backupsHandler.Create, service.VerbBackupWrite, service.KindTile, "id")
	mutate(site, "POST", "/backups/:id/save", backupsHandler.Save, service.VerbBackupWrite, service.KindBackup, "id")
	mutate(site, "POST", "/backups/:id/delete", backupsHandler.Delete, service.VerbBackupWrite, service.KindBackup, "id")
	mutate(site, "POST", "/backups/:id/run", backupsHandler.Run, service.VerbBackupWrite, service.KindBackup, "id")
	mutate(site, "POST", "/backups/:id/restore", backupsHandler.Restore, service.VerbBackupWrite, service.KindBackup, "id")
	read(site, "/backups/:id/history", backupsHandler.History, service.VerbOrgRead, service.KindBackup, "id")

	// The admin area: everything scoped to this installation. Personal settings
	// live under /account and org settings on the org's own page, /settings
	// used to be all three at once.
	read(site, "/admin", settingsHandler.Index, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/admin/users", settingsHandler.Users, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/admin/registries", settingsHandler.Registries, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/admin/tls", settingsHandler.TLS, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/admin/audit", settingsHandler.Audit, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/admin/maintenance", settingsHandler.Maintenance, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/admin/update", settingsHandler.Update, service.VerbAdminRead, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/update", settingsHandler.RunUpdate, service.VerbServerDefaults, service.KindNone, "", adminOnly)
	read(site, "/admin/update/check", settingsHandler.UpdateCheck, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/admin/update/badge", settingsHandler.UpdateBadge, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/admin/backups", settingsHandler.Backups, service.VerbAdminRead, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/backups/destinations", settingsHandler.CreateDestination, service.VerbServerDefaults, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/backups/destinations/:destID/delete", settingsHandler.DeleteDestination, service.VerbServerDefaults, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/backups/panel", settingsHandler.SavePanelBackup, service.VerbServerDefaults, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/backups/panel/run", settingsHandler.RunPanelBackup, service.VerbServerDefaults, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/backups/panel/delete", settingsHandler.DeletePanelBackup, service.VerbServerDefaults, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/registry/domain", settingsHandler.SetRegistryDomain, service.VerbServerDefaults, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/registries", settingsHandler.CreateRegistry, service.VerbServerDefaults, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/registries/:id/delete", settingsHandler.DeleteRegistry, service.VerbServerDefaults, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/cleanup", settingsHandler.ToggleCleanup, service.VerbServerDefaults, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/backups/destinations/:destID/shared", settingsHandler.ToggleDestinationShared, service.VerbServerDefaults, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/imagewatch", settingsHandler.SaveImageWatch, service.VerbServerDefaults, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/dns", settingsHandler.SaveDNS, service.VerbServerDefaults, service.KindNone, "", adminOnly)
	read(site, "/admin/proxy", settingsHandler.ProxyPage, service.VerbAdminRead, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/proxy/trusted", settingsHandler.SaveTrustedProxies, service.VerbProxyAdmin, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/proxy/override", settingsHandler.SaveProxyOverride, service.VerbProxyAdmin, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/proxy/entry", settingsHandler.SaveProxyEntry, service.VerbProxyAdmin, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/proxy/entry/delete", settingsHandler.DeleteProxyEntry, service.VerbProxyAdmin, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/users/:id/toggle", settingsHandler.ToggleUserActive, service.VerbUserAdmin, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/admin/users/:id/admin", settingsHandler.ToggleUserAdmin, service.VerbUserAdmin, service.KindNone, "", adminOnly)
	// Connectors are org-scoped: the pages live on the org, the handlers ship
	// with the admin package because that is where they grew up.
	// Outside orgSettings on purpose: the wizard's repo picker calls both while
	// setup is still open. Owner-only and scoped to this org's own connectors.
	read(site, "/orgs/:slug/settings/connectors/:connectorID/branches", orgHandler.ConnectorBranches, service.VerbOrgOwnerRead, service.KindOrg, "slug")
	read(site, "/orgs/:slug/settings/connectors/:connectorID/file", orgHandler.ConnectorFileExists, service.VerbOrgOwnerRead, service.KindOrg, "slug")
	// GitHub is told this callback path when the app manifest is created, so it
	// stays put even though the rest of /settings moved.
	site.GET("/settings/github/callback", settingsHandler.GitHubCallback, auth.RequireAuth())
	// Old bookmarks: admins to the admin area, everyone else to their account,
	// which is where the only part they could use (API keys) now lives.
	site.GET("/settings", settingsHandler.LegacyRedirect, auth.RequireAuth())

	accountHandler := accountpage.NewHandler(deps.Store, deps.AuthService, deps.FileStorage)

	// Uploaded avatars and org logos, streamed from FileStorage.
	site.GET("/avatars/*", avatar.Serve(deps.FileStorage), auth.RequireAuth())
	site.GET("/account", accountHandler.Index, auth.RequireAuth())
	site.GET("/account/profile", accountHandler.Profile, auth.RequireAuth())
	site.POST("/account/profile", accountHandler.SaveProfile, auth.RequireAuth())
	site.POST("/account/password", accountHandler.ChangePassword, auth.RequireAuth())
	site.GET("/account/apikeys", accountHandler.APIKeys, auth.RequireAuth())
	site.POST("/account/apikeys", accountHandler.CreateAPIKey, auth.RequireAuth())
	site.POST("/account/apikeys/:id/delete", accountHandler.DeleteAPIKey, auth.RequireAuth())
	site.GET("/account/notifications", accountHandler.Notifications, auth.RequireAuth())
	site.POST("/account/notifications", accountHandler.SaveNotifications, auth.RequireAuth())
	site.GET("/account/appearance", accountHandler.Appearance, auth.RequireAuth())
	site.POST("/account/appearance", accountHandler.SaveAppearance, auth.RequireAuth())
	site.GET("/account/graph-prefs", accountHandler.GraphPrefs, auth.RequireAuth())
	site.PUT("/account/graph-prefs", accountHandler.SaveGraphPrefs, auth.RequireAuth())

	notificationHandler := notificationpage.NewHandler(deps.Store).WithNotifications(deps.Notifications)
	site.GET("/notifications", notificationHandler.Page, auth.RequireAuth())
	site.GET("/notifications/badge", notificationHandler.Badge, auth.RequireAuth())
	site.POST("/notifications/read", notificationHandler.MarkAllRead, auth.RequireAuth())
	site.POST("/notifications/clear", notificationHandler.Clear, auth.RequireAuth())

	// CLI login (gh-style loopback). Authorize pages run behind the web session;
	// exchange is unauthenticated on the root group, the one-time code is the
	// only credential, and it must sit outside both KeyAuth and RequireAuth.
	cliHandler := clipage.NewHandler(deps.Store)
	site.GET("/cli/authorize", cliHandler.Authorize, auth.RequireAuth())
	site.POST("/cli/authorize", cliHandler.Approve, auth.RequireAuth())
	e.POST("/cli/exchange", cliHandler.Exchange, authLimit)

	containerHandler := containerpage.NewHandler(deps.Cluster, deps.Notifier, deps.Containers)
	read(site, "/containers", containerHandler.List, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/containers/node/:id", containerHandler.NodeList, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/containers/:id", containerHandler.Detail, service.VerbAdminRead, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/containers/:id/start", containerHandler.Start, service.VerbContainerAdmin, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/containers/:id/stop", containerHandler.Stop, service.VerbContainerAdmin, service.KindNone, "", adminOnly)
	mutate(site, "POST", "/containers/:id/remove", containerHandler.Remove, service.VerbContainerAdmin, service.KindNone, "", adminOnly)
	read(site, "/containers/:id/logs", containerHandler.LogsPage, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/containers/:id/logs/stream", containerHandler.LogsStream, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/containers/:id/term", containerHandler.TermPage, service.VerbAdminRead, service.KindNone, "", adminOnly)
	read(site, "/containers/:id/term/ws", containerHandler.TermWS, service.VerbAdminRead, service.KindNone, "", adminOnly)

	appHandler := apppage.NewHandler(deps.Store, deps.Cluster, deps.Proxy, deps.Engine, deps.Jobs, deps.GitHub, deps.Notifier, deps.Lifecycle, deps.Tiles, deps.Telemetry, deps.Domains).
		WithEnvironments(deps.Environments).WithStacks(deps.Stacks).WithOrgs(deps.Orgs).
		WithStorage(deps.Storage).WithAudit(deps.Audit).WithConnectors(deps.Connectors).
		WithNodeService(deps.NodeService).
		WithSlices(deps.Slices).
		WithVariables(deps.Variables).
		WithDeploys(deps.Deploys).
		WithGate(deps.Gate).
		WithScheduler(deps.Scheduler)
	read(site, "/apps/:id", redirectTile(deps.Store), service.VerbTileRead, service.KindTile, "id")
	read(site, "/apps/:id/branches", appHandler.Branches, service.VerbTileRead, service.KindTile, "id")
	read(site, "/apps/:id/connectors", appHandler.Connectors, service.VerbTileRead, service.KindTile, "id")
	read(site, "/apps/:id/repos", appHandler.Repos, service.VerbTileRead, service.KindTile, "id")
	read(site, "/apps/:id/panel", appHandler.Panel, service.VerbTileRead, service.KindTile, "id")
	read(site, "/apps/:id/panel/content", appHandler.PanelContent, service.VerbTileRead, service.KindTile, "id")
	read(site, "/apps/:id/panel/header", appHandler.PanelHeader, service.VerbTileRead, service.KindTile, "id")
	read(site, "/apps/:id/metrics", appHandler.Metrics, service.VerbTileRead, service.KindTile, "id")
	read(site, "/apps/:id/logs/stream", appHandler.LogsStream, service.VerbTileRead, service.KindTile, "id")
	read(site, "/apps/:id/vars", appHandler.Vars, service.VerbTileRead, service.KindTile, "id")
	read(site, "/apps/:id/vars/value", appHandler.VarValue, service.VerbVariableWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/vars/secret", appHandler.SaveSecretVar, service.VerbVariableWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/vars/delete", appHandler.DeleteVar, service.VerbVariableWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/env", appHandler.SaveEnv, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/settings", appHandler.SaveSettings, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/attach", appHandler.Attach, service.VerbTileWrite, service.KindTile, "id")
	read(site, "/apps/:id/volume/files", appHandler.VolumeFiles, service.VerbTileRead, service.KindTile, "id")
	read(site, "/apps/:id/volume/files/download", appHandler.VolumeFileDownload, service.VerbTileRead, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/volume/files/upload", appHandler.VolumeFileUpload, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/volume/files/delete", appHandler.VolumeFileDelete, service.VerbTileWrite, service.KindTile, "id")
	read(site, "/apps/:id/provisions", appHandler.Provisions, service.VerbTileRead, service.KindTile, "id")
	read(site, "/apps/:id/size", appHandler.Size, service.VerbTileRead, service.KindTile, "id")
	read(site, "/apps/:id/storage", appHandler.StorageFrag, service.VerbTileRead, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/storage", appHandler.AttachStorage, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/storage/detach", appHandler.DetachStorage, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/provision", appHandler.Provision, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/provisions/attach", appHandler.AttachProvision, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/provisions/:pid/detach", appHandler.DetachProvision, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/delete", appHandler.Delete, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/domains", appHandler.CreateDomain, service.VerbDomainWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/domains/auto", appHandler.CreateAutoDomain, service.VerbDomainWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/domains/:domainID/https", appHandler.ToggleDomainHTTPS, service.VerbDomainWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/domains/:domainID/cert", appHandler.SetDomainCert, service.VerbDomainWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/domains/:domainID/delete", appHandler.DeleteDomain, service.VerbDomainWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/stop", appHandler.Stop, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/restart", appHandler.Restart, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/run", appHandler.RunNow, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/runs/:run/stop", appHandler.StopRun, service.VerbTileWrite, service.KindTile, "id")
	read(site, "/apps/:id/runs/logs/stream", appHandler.RunsLogsStream, service.VerbTileRead, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/cron/toggle", appHandler.ToggleCron, service.VerbTileWrite, service.KindTile, "id")

	deploymentHandler := deploymentpage.NewHandler(deps.Store, deps.Engine, deps.StreamHub).WithDeploys(deps.Deploys).WithTiles(deps.Tiles)
	mutate(site, "POST", "/apps/:id/deploy", deploymentHandler.Deploy, service.VerbTileWrite, service.KindTile, "id")
	mutate(site, "POST", "/apps/:id/rollback", deploymentHandler.Rollback, service.VerbTileWrite, service.KindTile, "id")
	read(site, "/deployments/:id", deploymentHandler.Detail, service.VerbDeploymentRead, service.KindDeployment, "id")
	read(site, "/deployments/:id/status", deploymentHandler.Status, service.VerbDeploymentRead, service.KindDeployment, "id")
	read(site, "/deployments/:id/stream", deploymentHandler.Stream, service.VerbDeploymentRead, service.KindDeployment, "id")
	mutate(site, "POST", "/deployments/:id/cancel", deploymentHandler.Cancel, service.VerbDeploymentCancel, service.KindDeployment, "id")

	// Webhooks: outside the site group, token/signature-authenticated, no CSRF/session.
	prHandler := prhook.NewHandler(deps.Store, deps.Engine,
		envops.Ops{Store: deps.Store, RT: deps.Runtime, Cluster: deps.Cluster, PX: deps.Proxy, DBs: deps.Databases,
			Tiles: deps.Tiles, Sched: deps.Scheduler, Domains: deps.Domains, Resources: deps.Resources}, applier, deps.GitHub, deps.Notifier).
		WithEnvironments(deps.Environments).
		WithStacks(deps.Stacks).
		WithTiles(deps.Tiles).
		WithOrgs(deps.Orgs).
		WithSettings(deps.Settings).
		WithConnectors(deps.Connectors).
		WithPREnvs(deps.PREnvs).
		WithOrgConfig(deps.OrgConfig).
		WithWork(deps.Work).
		WithScheduler(deps.Scheduler)
	e.POST("/hooks/github/:stack", prHandler.Hook, middleware.Logging())
	e.POST("/hooks/connectors/:id", prHandler.HookConnector, middleware.Logging())

	// Auth routes, one page-package per page (login owns logout as its inverse action).
	loginHandler := login.NewHandler(deps.AuthService, deps.SessionManager)
	// The mirror of firstBootOnly: with no accounts yet there is nothing to log
	// in to, so the login page sends the first visitor to registration instead
	// of asking for credentials that cannot exist.
	firstBootRegister := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			n, err := deps.Store.CountUsers(c.Request().Context())
			if err != nil {
				return err
			}
			if n == 0 {
				return c.Redirect(http.StatusSeeOther, "/register")
			}
			return next(c)
		}
	}
	site.GET("/login", loginHandler.Page, auth.RequireNotAuth(), firstBootRegister)
	site.POST("/login", loginHandler.Submit, auth.RequireNotAuth(), authLimit)
	site.POST("/login/validate/:field", loginHandler.FormRules.ValidationHandler("field"), auth.RequireNotAuth())
	site.POST("/logout", loginHandler.Logout, auth.RequireAuth())

	// Registration is first-boot only: once a user exists, invites go through
	// the (logged-in) users settings page instead.
	firstBootOnly := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			n, err := deps.Store.CountUsers(c.Request().Context())
			if err != nil {
				return err
			}
			if n > 0 {
				return c.Redirect(http.StatusSeeOther, "/login")
			}
			return next(c)
		}
	}
	registerHandler := register.NewHandler(deps.AuthService, deps.SessionManager)
	site.GET("/register", registerHandler.Page, auth.RequireNotAuth(), firstBootOnly)
	site.POST("/register", registerHandler.Submit, auth.RequireNotAuth(), firstBootOnly)
	site.POST("/register/validate/:field", registerHandler.FormRules.ValidationHandler("field"), auth.RequireNotAuth(), firstBootOnly)

	// Org invite links: logged-in users join directly, visitors register through
	// the invite. Works both ways, so no auth requirement either direction.
	inviteHandler := invite.NewHandler(deps.Store, deps.AuthService, deps.SessionManager).WithOrgs(deps.Orgs).WithMembers(deps.Members)
	site.GET("/invite/:token", inviteHandler.Page)
	site.POST("/invite/:token", inviteHandler.Submit, authLimit)

	// One-time secret links. No auth either way, the token is the credential,
	// and the person on the other end has no account by design. The rate limit
	// is per-IP: the token itself is unguessable and passphrase attempts are
	// capped per link, so this only exists to blunt scripted hammering.
	shareHandler := sharepub.NewHandler(deps.Store).WithVariables(deps.Variables)
	shareLimit := hamrmw.RateLimitWithConfig(hamrmw.RateLimitConfig{
		Store:  hamrmw.NewMemoryStore(hamrmw.WithMaxSize(10000)),
		Rate:   30,
		Window: time.Minute,
	})
	site.GET("/s/:token", shareHandler.Page, shareLimit)
	site.POST("/s/:token", shareHandler.Submit, shareLimit)

	// Canonical slug URLs: /:org/:stack[/:env[/:tile]]. Registered last;
	// echo prefers static segments, so reserved top-level paths always win.
	read(site, "/:org/:stack", projectHandler.StackGraph, service.VerbOrgRead, service.KindOrg, "org")
	// Stack settings: one URL per section, plus a page per environment. These
	// were all one long scroll, with environment settings hidden in disclosures.
	read(site, "/:org/:stack/plans/:planID", projectHandler.PlanView, service.VerbOrgRead, service.KindOrg, "org")
	read(site, "/:org/:stack/releases", projectHandler.Releases, service.VerbOrgRead, service.KindOrg, "org")
	read(site, "/:org/:stack/settings", projectHandler.Settings, service.VerbOrgRead, service.KindOrg, "org")
	read(site, "/:org/:stack/settings/general", projectHandler.SettingsGeneral, service.VerbOrgRead, service.KindOrg, "org")
	read(site, "/:org/:stack/settings/config", projectHandler.SettingsConfig, service.VerbOrgRead, service.KindOrg, "org")
	read(site, "/:org/:stack/settings/config/export", projectHandler.ExportConfig, service.VerbConfigExport, service.KindOrg, "org")
	read(site, "/:org/:stack/settings/variables", projectHandler.SettingsVariables, service.VerbOrgRead, service.KindOrg, "org")
	read(site, "/:org/:stack/settings/variables/panel", projectHandler.StackVarsPanel, service.VerbOrgRead, service.KindOrg, "org")
	read(site, "/:org/:stack/settings/variables/value", projectHandler.StackVarValue, service.VerbVariableWrite, service.KindOrg, "org")
	read(site, "/:org/:stack/settings/domains", projectHandler.SettingsDomains, service.VerbOrgRead, service.KindOrg, "org")
	read(site, "/:org/:stack/settings/environments", projectHandler.SettingsEnvironments, service.VerbOrgRead, service.KindOrg, "org")
	read(site, "/:org/:stack/settings/environments/:env", projectHandler.SettingsEnvironment, service.VerbOrgRead, service.KindOrg, "org")
	read(site, "/:org/:stack/settings/environments/:env/variables/panel", projectHandler.EnvVarsPanel, service.VerbOrgRead, service.KindOrg, "org")
	read(site, "/:org/:stack/settings/environments/:env/variables/value", projectHandler.EnvVarValue, service.VerbVariableWrite, service.KindOrg, "org")
	read(site, "/:org/:stack/settings/pr", projectHandler.SettingsPREnv, service.VerbOrgRead, service.KindOrg, "org")
	read(site, "/:org/:stack/:env", projectHandler.Graph, service.VerbOrgRead, service.KindOrg, "org")
	read(site, "/:org/:stack/:env/logs", projectHandler.EnvLogs, service.VerbOrgRead, service.KindOrg, "org")
	read(site, "/:org/:stack/:env/:tile", tilePage(deps.Store, appHandler.Detail, dbHandler.Detail), service.VerbOrgRead, service.KindOrg, "org")
}

// tilePage resolves /:org/:stack/:env/:tile to a tile and delegates to the
// app or db detail handler with the :id param rewritten to the tile's id.
func tilePage(store repo.Store, appDetail, dbDetail echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		org, err := store.GetOrgBySlug(ctx, c.Param("org"))
		if err != nil || org == nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		stack, err := store.GetStackBySlug(ctx, org.ID, c.Param("stack"))
		if err != nil || stack == nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		env, err := store.GetEnvironmentBySlug(ctx, stack.ID, c.Param("env"))
		if err != nil || env == nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		t, err := store.GetTileBySlug(ctx, env.ID, c.Param("tile"))
		if err != nil || t == nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		c.SetParamNames("id")
		c.SetParamValues(t.ID)
		if t.IsManaged() {
			return dbDetail(c)
		}
		return appDetail(c)
	}
}

// clientIP is the caller's own address. Traefik is the edge here and replaces
// any inbound X-Forwarded-For, so the leftmost entry is the real client.
func clientIP(c echo.Context) string {
	if xff := c.Request().Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	return c.RealIP()
}

// redirectTile sends legacy /apps/:id and /dbs/:id URLs to the canonical
// slug URL, preserving the query string (?tab=...).
func redirectTile(store repo.Store) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		t, err := store.GetTile(ctx, c.Param("id"))
		if err != nil || t == nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		// Was unguarded: the redirect target spells out the org, stack, env and
		// tile slugs, so any logged-in user could walk tile ids and read them.
		if err := middleware.RequireStackAccess(c, store, t.StackID); err != nil {
			return err
		}
		env, err := store.GetEnvironment(ctx, t.EnvironmentID)
		if err != nil || env == nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		stack, err := store.GetStack(ctx, t.StackID)
		if err != nil || stack == nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		org, err := store.GetOrg(ctx, stack.OrgID)
		if err != nil || org == nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		url := "/" + org.Slug + "/" + stack.Slug + "/" + env.Slug + "/" + t.Slug
		if q := c.QueryString(); q != "" {
			url += "?" + q
		}
		return c.Redirect(http.StatusSeeOther, url)
	}
}

// RegisterStaticPages registers handlers for static generation and runtime
// serving. Each call to StaticPage registers both a generation entry and a
// GET route. These handlers must not depend on database or session state.
func RegisterStaticPages(srv *server.Server) {
	h := about.NewHandler()
	srv.StaticPage("/about", h.About)
}

// PollHeader marks a request htmx made on a timer rather than because someone
// did something. Set by the htmx:configRequest hook in
// frontend/static/js/main.js for any element whose hx-trigger has an interval.
const PollHeader = "X-Stackr-Poll"

// skipOnPoll runs mw only for requests a person caused.
//
// The flash middleware clears the cookie the moment it reads it, and several
// pages refresh themselves on a timer against their own full-page handler with
// hx-select. The whole page renders, the flash is consumed, and then everything
// but the selected fragment is discarded, flash slot included. So any message
// any handler set was destroyed within the poll interval of the operator
// sitting on that page, whether or not it had anything to do with that page.
//
// A poll renders no flash, so it has no business consuming one.
func skipOnPoll(mw echo.MiddlewareFunc) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		wrapped := mw(next)
		return func(c echo.Context) error {
			if c.Request().Header.Get(PollHeader) == "1" {
				return next(c)
			}
			return wrapped(c)
		}
	}
}
