// Package web is the HTML surface: the full route table, the middleware groups
// behind it, and the shared error rendering. Every page's handler and templates
// live under web/handler in a package that mirrors its URL path, and the
// components they share live under web/components.
package web

import (
	"context"
	"net"
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
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/notify"
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
	"github.com/FyrmForge/stackr/internal/stackrd/infra/mail"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/metrics"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/nodes"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	stackruntime "github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/volmove"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Deps holds the dependencies for route registration.
type Deps struct {
	Store   repo.Store
	BaseURL string
	// CookieDomain is what the session cookie is scoped to (BASE_URL's host).
	CookieDomain  string
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
	Proxy          *proxy.Proxy
	Databases      *managedtiles.Service
	Jobs           *jobs.Service
	Backups        *backup.Service
	Metrics        *metrics.Sampler
	Forwards       *forward.Registry
	GitHub         *githubapp.Client
	// Mail is nil when no provider is configured: invites then fall back to
	// copy-the-link, which is the only channel a self-hosted box always has.
	Mail *mail.Mailer
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
			return deps.Store.GetUserByID(reqCtx, id)
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

	settingsHandler := settingspage.NewHandler(deps.Store, deps.Backups, deps.Admin, deps.Runtime, deps.Proxy, deps.GitHub, deps.RegistrySigner, deps.DataDir, deps.RegistryPort, deps.ACMEEmail, deps.BaseURL)

	searchHandler := searchpage.NewHandler(deps.Store)
	site.GET("/search", searchHandler.Search, auth.RequireAuth())

	orgHandler := orgpage.NewHandler(deps.Store, deps.Notifier, deps.Metrics, deps.FileStorage, deps.Runtime, deps.Forwards, deps.OrgConfig, deps.GitHub, deps.Mail, deps.RegistrySigner)
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
	site.GET("/setup", orgHandler.SetupStart, auth.RequireAuth(), adminOnly)
	site.GET("/orgs/:slug/setup/:step", orgHandler.Setup, auth.RequireAuth())
	// The wizard's own POST routes. Each is served by the handler that owns the
	// same change in settings; arriving here is what makes it redirect back to
	// the step instead of to the settings tab (backTo in handler/org/setup.go).
	site.POST("/orgs/:slug/setup/name", orgHandler.Rename, auth.RequireAuth())
	site.POST("/orgs/:slug/setup/mode", orgHandler.SetupMode, auth.RequireAuth())
	site.POST("/orgs/:slug/setup/connector", settingsHandler.GitHubConnect, auth.RequireAuth())
	site.POST("/orgs/:slug/setup/config", orgHandler.SaveOrgConfig, auth.RequireAuth())
	site.GET("/orgs/:slug/setup/config/plan", orgHandler.SetupConfigPlan, auth.RequireAuth())
	site.POST("/orgs/:slug/setup/config/plan/:planID/approve", orgHandler.SetupApprovePlan, auth.RequireAuth())
	site.POST("/orgs/:slug/setup/config/plan/:planID/reject", orgHandler.SetupRejectPlan, auth.RequireAuth())
	site.POST("/orgs/:slug/setup/config/plan/:planID/inputs", orgHandler.SetPlanInput, auth.RequireAuth())
	site.POST("/orgs/:slug/setup/domain", orgHandler.SaveOrgDomain, auth.RequireAuth())
	site.POST("/orgs/:slug/setup/team/members", orgHandler.AddMember, auth.RequireAuth())
	site.POST("/orgs/:slug/setup/team/members/:userID/role", orgHandler.SetMemberRole, auth.RequireAuth())
	site.POST("/orgs/:slug/setup/team/members/:userID/remove", orgHandler.RemoveMember, auth.RequireAuth())
	site.POST("/orgs/:slug/setup/team/invites/:inviteID/resend", orgHandler.ResendInvite, auth.RequireAuth())
	site.POST("/orgs/:slug/setup/team/invites/:inviteID/reinvite", orgHandler.ReinviteMember, auth.RequireAuth())
	site.POST("/orgs/:slug/setup/team/invites/:inviteID/delete", orgHandler.DeleteInvite, auth.RequireAuth())
	site.POST("/orgs/:slug/setup/done", orgHandler.SetupDone, auth.RequireAuth())
	site.GET("/orgs/:slug", orgHandler.Graph, auth.RequireAuth())
	site.GET("/orgs/:slug/graph/status", orgHandler.GraphStatus, auth.RequireAuth())
	site.POST("/orgs/:slug/graph/positions", orgHandler.SaveNodePosition, auth.RequireAuth())
	site.POST("/orgs/:slug/graph/positions/reset", orgHandler.ResetNodePositions, auth.RequireAuth())
	site.POST("/orgs/:slug/graph/annotations", orgHandler.SaveAnnotation, auth.RequireAuth())
	site.POST("/orgs/:slug/graph/annotations/delete", orgHandler.DeleteAnnotation, auth.RequireAuth())
	site.POST("/orgs/:slug/graph/groups", orgHandler.SaveGraphGroup, auth.RequireAuth())
	site.POST("/orgs/:slug/graph/groups/delete", orgHandler.DeleteGraphGroup, auth.RequireAuth())
	// The org's settings page: members, invites, connectors, variables, rename
	// and delete. ":id" accepts the org's slug or its uuid.
	site.GET("/orgs/:slug/plans", orgHandler.Plans, auth.RequireAuth())
	site.GET("/orgs/:slug/plans/:planID", orgHandler.OrgPlanView, auth.RequireAuth())
	site.POST("/orgs/:slug/plans/:planID/approve", orgHandler.ApproveOrgPlan, auth.RequireAuth())
	site.POST("/orgs/:slug/plans/:planID/reject", orgHandler.RejectOrgPlan, auth.RequireAuth())
	site.POST("/orgs/:slug/plans/:planID/inputs", orgHandler.SetPlanInput, auth.RequireAuth())
	// Settings are closed until the org has been through the wizard, so the
	// steps cannot be skipped sideways by clicking into a tab. Everything that
	// has to stay reachable mid-setup (the canvas, the plans pages, the repo
	// picker's lookups) is registered outside this group.
	orgSettings := site.Group("/orgs/:slug/settings", auth.RequireAuth(), middleware.RequireSetupDone(deps.Store))
	orgSettings.GET("", orgHandler.SettingsIndex)
	orgSettings.GET("/general", orgHandler.SettingsGeneral)
	orgSettings.GET("/members", orgHandler.SettingsMembers)
	orgSettings.GET("/invites", orgHandler.SettingsInvites)
	orgSettings.GET("/connectors", orgHandler.SettingsConnectors)
	orgSettings.GET("/variables", orgHandler.SettingsVariables)
	orgSettings.GET("/variables/panel", orgHandler.VarsPanel)
	orgSettings.GET("/variables/value", orgHandler.OrgVarValue)
	orgSettings.GET("/domains", orgHandler.SettingsDomains)
	orgSettings.POST("/domains", orgHandler.SaveOrgDomain)
	orgSettings.POST("/domains/delete", orgHandler.DeleteOrgDomain)
	orgSettings.GET("/storage", orgHandler.SettingsStorage)
	orgSettings.POST("/env-colors", orgHandler.SaveEnvColor)
	orgSettings.GET("/backups", orgHandler.SettingsBackups)
	orgSettings.GET("/config/export", orgHandler.ExportConfig)
	orgSettings.GET("/defaults", orgHandler.SettingsDefaults)
	orgSettings.POST("/defaults", orgHandler.SaveDefaults)
	orgSettings.GET("/registry", orgHandler.SettingsRegistry)
	orgSettings.GET("/registry/images", orgHandler.RegistryImages)
	orgSettings.POST("/registry/credentials", orgHandler.CreateRegistryCredential)
	orgSettings.POST("/registry/credentials/delete", orgHandler.DeleteRegistryCredential)
	orgSettings.POST("/registry/tags/delete", orgHandler.DeleteRegistryTag)
	orgSettings.GET("/config", orgHandler.SettingsConfig)
	orgSettings.POST("/config", orgHandler.SaveOrgConfig)
	orgSettings.POST("/backups/destinations", settingsHandler.CreateDestination)
	orgSettings.POST("/backups/destinations/:destID/delete", settingsHandler.DeleteDestination)
	orgSettings.POST("/vars", orgHandler.SaveOrgVar)
	orgSettings.POST("/vars/delete", orgHandler.DeleteOrgVar)
	orgSettings.POST("/connectors/github", settingsHandler.GitHubConnect)
	orgSettings.POST("/connectors/:connectorID/delete", settingsHandler.DeleteConnector)
	site.POST("/orgs/:slug/rename", orgHandler.Rename, auth.RequireAuth())
	site.POST("/orgs/:slug/delete", orgHandler.Delete, auth.RequireAuth())
	site.POST("/orgs/:slug/members", orgHandler.AddMember, auth.RequireAuth())
	site.POST("/orgs/:slug/members/:userID/role", orgHandler.SetMemberRole, auth.RequireAuth())
	site.POST("/orgs/:slug/members/:userID/remove", orgHandler.RemoveMember, auth.RequireAuth())
	site.POST("/orgs/:slug/invites", orgHandler.CreateInvite, auth.RequireAuth())
	site.POST("/orgs/:slug/invites/:inviteID/delete", orgHandler.DeleteInvite, auth.RequireAuth())
	site.POST("/orgs/:slug/invites/:inviteID/resend", orgHandler.ResendInvite, auth.RequireAuth())
	site.POST("/orgs/:slug/invites/:inviteID/reinvite", orgHandler.ReinviteMember, auth.RequireAuth())
	site.POST("/orgs", orgHandler.Create, auth.RequireAuth(), adminOnly)
	site.POST("/orgs/switch", orgHandler.Switch, auth.RequireAuth())
	site.POST("/orgs/stacks/:id/move", orgHandler.MoveStack, auth.RequireAuth())

	serverHandler := serverpage.NewHandler(serverpage.Deps{
		Store: deps.Store, Runtime: deps.Runtime, Proxy: deps.Proxy,
		CookieDomain: deps.CookieDomain, Nodes: deps.Nodes, Cluster: deps.Cluster,
		Mover: deps.Mover, BaseURL: deps.BaseURL,
		DataDir: deps.DataDir, Version: deps.Version,
	})
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
	site.GET("/servers", serverHandler.List, auth.RequireAuth(), adminOnly)
	site.GET("/servers/add", serverHandler.AddNodeForm, auth.RequireAuth(), adminOnly)
	site.POST("/servers/add", serverHandler.AddNode, auth.RequireAuth(), adminOnly)
	site.GET("/servers/move/:id", serverHandler.MoveForm, auth.RequireAuth(), adminOnly)
	site.POST("/servers/move/:id", serverHandler.Move, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/join-key", serverHandler.NewJoinKey, auth.RequireAuth(), adminOnly)
	site.GET("/servers/:id/drain", serverHandler.DrainForm, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/drain", serverHandler.Drain, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/activate", serverHandler.Activate, auth.RequireAuth(), adminOnly)
	site.GET("/servers/:id/remove", serverHandler.RemoveForm, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/remove", serverHandler.Remove, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/group", serverHandler.SaveGroup, auth.RequireAuth(), adminOnly)
	site.GET("/servers/:id", serverHandler.Detail, auth.RequireAuth(), adminOnly)
	site.GET("/servers/:id/host", serverHandler.Host, auth.RequireAuth(), adminOnly)
	site.GET("/servers/:id/volumes", serverHandler.Volumes, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/settings", serverHandler.SaveSettings, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/volumes", serverHandler.CreateVolume, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/volumes/delete", serverHandler.DeleteVolume, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/storage", serverHandler.CreateStorage, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/storage/delete", serverHandler.DeleteStorage, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/storage/probe", serverHandler.ProbeStorage, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/storage/paths", serverHandler.CreateStoragePath, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/storage/paths/delete", serverHandler.DeleteStoragePath, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/domains", serverHandler.CreateDomainResource, auth.RequireAuth(), adminOnly)
	site.POST("/servers/:id/domains/delete", serverHandler.DeleteDomainResource, auth.RequireAuth(), adminOnly)

	applier := deps.Applier
	projectHandler := project.NewHandler(deps.Store, deps.Jobs, deps.Runtime, deps.Cluster, deps.Proxy, deps.Metrics, deps.Notifier, deps.GitHub, applier, deps.Forwards).
		WithMover(deps.Mover).
		WithWork(deps.Work)
	site.POST("/projects", projectHandler.Create, auth.RequireAuth())
	// Legacy id URLs redirect to the canonical slug URLs.
	site.GET("/projects/:id", projectHandler.RedirectStack, auth.RequireAuth())
	site.GET("/projects/:id/graph", projectHandler.RedirectStack, auth.RequireAuth())
	site.POST("/projects/:id/delete", projectHandler.Delete, auth.RequireAuth())
	site.POST("/projects/:id/settings", projectHandler.SaveSettings, auth.RequireAuth())
	site.POST("/projects/:id/config", projectHandler.SaveConfigBinding, auth.RequireAuth())
	site.POST("/projects/:id/config/plan", projectHandler.PlanNow, auth.RequireAuth())
	// The pre-rename URL, kept alive because links to it exist; the page
	// itself now lives under /:org/:stack.
	site.GET("/projects/:id/config/plans/:planID", projectHandler.PlanRedirect, auth.RequireAuth())
	site.POST("/projects/:id/config/plans/:planID/approve", projectHandler.ApprovePlan, auth.RequireAuth())
	site.POST("/projects/:id/config/plans/:planID/reject", projectHandler.RejectPlan, auth.RequireAuth())
	site.POST("/projects/:id/config/plans/:planID/inputs", projectHandler.SetPlanInput, auth.RequireAuth())
	site.POST("/envs/:id/config", projectHandler.SaveEnvConfig, auth.RequireAuth())
	site.POST("/projects/:id/apps", projectHandler.CreateTile, auth.RequireAuth())
	site.GET("/projects/:id/repos", projectHandler.Repos, auth.RequireAuth())
	site.POST("/projects/:id/dbs", projectHandler.CreateDB, auth.RequireAuth())
	site.POST("/projects/:id/envs", projectHandler.CreateEnvironment, auth.RequireAuth())
	site.POST("/envs/:id/delete", projectHandler.DeleteEnvironment, auth.RequireAuth())
	site.POST("/envs/:id/reset", projectHandler.ResetEnvironment, auth.RequireAuth())
	site.POST("/envs/:id/settings", projectHandler.SaveEnvSettings, auth.RequireAuth())
	site.POST("/envs/:id/color", projectHandler.SaveEnvColor, auth.RequireAuth())
	site.POST("/envs/:id/vars", projectHandler.SaveEnvVar, auth.RequireAuth())
	site.POST("/envs/:id/vars/delete", projectHandler.DeleteEnvVar, auth.RequireAuth())
	site.POST("/projects/:id/vars", projectHandler.SaveStackVar, auth.RequireAuth())
	site.POST("/projects/:id/vars/delete", projectHandler.DeleteStackVar, auth.RequireAuth())
	site.POST("/projects/:id/links", projectHandler.MintStackLink, auth.RequireAuth())
	site.POST("/projects/:id/links/revoke", projectHandler.RevokeStackLink, auth.RequireAuth())
	site.POST("/projects/:id/domain-resources", projectHandler.SaveStackDomain, auth.RequireAuth())
	site.POST("/projects/:id/domain-resources/delete", projectHandler.DeleteStackDomain, auth.RequireAuth())
	site.POST("/projects/:id/prenv", projectHandler.SavePREnv, auth.RequireAuth())
	site.POST("/projects/:id/prenv/rotate", projectHandler.RotatePRSecret, auth.RequireAuth())
	// Environment-scoped canvas data (graph.js reads these off data attrs).
	site.GET("/envs/:id/graph/status", projectHandler.GraphStatus, auth.RequireAuth())
	site.GET("/projects/:id/staging/:envID", projectHandler.StagingReview, auth.RequireAuth())
	site.POST("/projects/:id/staging/:envID/apply", projectHandler.StagingApply, auth.RequireAuth())
	site.POST("/projects/:id/staging/:envID/discard", projectHandler.StagingDiscard, auth.RequireAuth())
	site.POST("/projects/:id/staging/:envID/changes/:changeID/discard", projectHandler.StagingDiscardOne, auth.RequireAuth())
	site.GET("/envs/:id/logs/stream", projectHandler.EnvLogsStream, auth.RequireAuth())
	site.POST("/envs/:id/graph/positions", projectHandler.SaveNodePosition, auth.RequireAuth())
	site.POST("/envs/:id/graph/positions/reset", projectHandler.ResetNodePositions, auth.RequireAuth())
	site.POST("/envs/:id/graph/annotations", projectHandler.SaveEnvAnnotation, auth.RequireAuth())
	site.POST("/envs/:id/graph/annotations/delete", projectHandler.DeleteEnvAnnotation, auth.RequireAuth())
	site.POST("/envs/:id/graph/groups", projectHandler.SaveEnvGraphGroup, auth.RequireAuth())
	site.POST("/envs/:id/graph/groups/delete", projectHandler.DeleteEnvGraphGroup, auth.RequireAuth())
	// The environments pill and panel on the stack canvas (plan 24).
	site.GET("/projects/:id/envs/compare", projectHandler.EnvCompare, auth.RequireAuth())
	// Promotion: one dialogue, one endpoint, wherever the button sits (the
	// releases page, a config plan row). The GET renders the confirm as a
	// fragment because it carries the commits going in, not one sentence.
	site.GET("/projects/:id/envs/:slug/promote", projectHandler.PromoteDialogue, auth.RequireAuth())
	site.POST("/projects/:id/envs/:slug/promote", projectHandler.PromoteCommit, auth.RequireAuth())
	site.POST("/envs/:id/intended", projectHandler.MarkIntended, auth.RequireAuth())
	site.POST("/envs/:id/copy", projectHandler.CopyEnv, auth.RequireAuth())
	// Stack canvas: same three endpoints one level up (see StackGraph).
	site.GET("/projects/:id/graph/status", projectHandler.StackGraphStatus, auth.RequireAuth())
	site.POST("/projects/:id/graph/positions", projectHandler.SaveStackNodePosition, auth.RequireAuth())
	site.POST("/projects/:id/graph/positions/reset", projectHandler.ResetStackNodePositions, auth.RequireAuth())
	site.POST("/projects/:id/graph/annotations", projectHandler.SaveStackAnnotation, auth.RequireAuth())
	site.POST("/projects/:id/graph/annotations/delete", projectHandler.DeleteStackAnnotation, auth.RequireAuth())
	site.POST("/projects/:id/graph/groups", projectHandler.SaveStackGraphGroup, auth.RequireAuth())
	site.POST("/projects/:id/graph/groups/delete", projectHandler.DeleteStackGraphGroup, auth.RequireAuth())

	dbHandler := dbpage.NewHandler(deps.Store, deps.Databases, deps.Cluster, deps.Proxy, deps.Notifier)
	site.GET("/dbs/:id", redirectTile(deps.Store), auth.RequireAuth())
	site.GET("/dbs/:id/panel", dbHandler.Panel, auth.RequireAuth())
	site.GET("/dbs/:id/panel/content", dbHandler.PanelContent, auth.RequireAuth())
	site.GET("/dbs/:id/volume/panel", dbHandler.VolumePanel, auth.RequireAuth())
	site.GET("/dbs/:id/volume/files", dbHandler.VolumeFiles, auth.RequireAuth())
	site.GET("/dbs/:id/volume/files/download", dbHandler.VolumeFileDownload, auth.RequireAuth())
	site.POST("/dbs/:id/volume/files/upload", dbHandler.VolumeFileUpload, auth.RequireAuth())
	site.POST("/dbs/:id/volume/files/delete", dbHandler.VolumeFileDelete, auth.RequireAuth())
	site.GET("/dbs/:id/buckets/:bucket/panel", dbHandler.BucketPanel, auth.RequireAuth())
	site.GET("/dbs/:id/buckets/:bucket/files", dbHandler.BucketFiles, auth.RequireAuth())
	site.GET("/dbs/:id/buckets/:bucket/files/download", dbHandler.BucketFileDownload, auth.RequireAuth())
	site.POST("/dbs/:id/buckets/:bucket/files/upload", dbHandler.BucketFileUpload, auth.RequireAuth())
	site.POST("/dbs/:id/buckets/:bucket/files/delete", dbHandler.BucketFileDelete, auth.RequireAuth())
	site.GET("/dbs/:id/data", dbHandler.Data, auth.RequireAuth())
	site.GET("/dbs/:id/data/cell", dbHandler.DataCell, auth.RequireAuth())
	site.POST("/dbs/:id/data/update", dbHandler.DataUpdate, auth.RequireAuth())
	site.POST("/dbs/:id/data/insert", dbHandler.DataInsert, auth.RequireAuth())
	site.POST("/dbs/:id/data/delete", dbHandler.DataDelete, auth.RequireAuth())
	site.GET("/dbs/:id/pgdbs/:db/panel", dbHandler.PGDBPanel, auth.RequireAuth())
	site.GET("/dbs/:id/metrics", dbHandler.Metrics, auth.RequireAuth())
	site.GET("/dbs/:id/logs/stream", dbHandler.LogsStream, auth.RequireAuth())
	site.POST("/dbs/:id/deploy", dbHandler.Deploy, auth.RequireAuth())
	site.POST("/dbs/:id/stop", dbHandler.Stop, auth.RequireAuth())
	site.POST("/dbs/:id/start", dbHandler.Start, auth.RequireAuth())
	site.POST("/dbs/:id/delete", dbHandler.Delete, auth.RequireAuth())
	site.POST("/dbs/:id/port", dbHandler.SetPort, auth.RequireAuth())
	site.POST("/dbs/:id/scope", dbHandler.SetScope, auth.RequireAuth())
	site.POST("/dbs/:id/domains", dbHandler.CreateDomain, auth.RequireAuth())
	site.POST("/dbs/:id/domains/:domainID/delete", dbHandler.DeleteDomain, auth.RequireAuth())
	site.GET("/dbs/:id/provisions", dbHandler.Provisions, auth.RequireAuth())
	site.POST("/dbs/:id/provisions/drop", dbHandler.DropProvision, auth.RequireAuth())
	site.POST("/dbs/:id/provisions/public", dbHandler.SetProvisionPublic, auth.RequireAuth())
	site.POST("/dbs/:id/provisions/fork", dbHandler.ForkProvision, auth.RequireAuth())

	// The Backups tab, shared by database tiles and volume tiles: one fragment
	// both panel packages fetch, rather than the same markup twice.
	backupsHandler := backupspage.NewHandler(deps.Store, deps.Backups)
	site.GET("/tiles/:id/backups", backupsHandler.Panel, auth.RequireAuth())
	site.POST("/tiles/:id/backups", backupsHandler.Create, auth.RequireAuth())
	site.POST("/backups/:id/save", backupsHandler.Save, auth.RequireAuth())
	site.POST("/backups/:id/delete", backupsHandler.Delete, auth.RequireAuth())
	site.POST("/backups/:id/run", backupsHandler.Run, auth.RequireAuth())
	site.POST("/backups/:id/restore", backupsHandler.Restore, auth.RequireAuth())

	// The admin area: everything scoped to this installation. Personal settings
	// live under /account and org settings on the org's own page, /settings
	// used to be all three at once.
	site.GET("/admin", settingsHandler.Index, auth.RequireAuth(), adminOnly)
	site.GET("/admin/users", settingsHandler.Users, auth.RequireAuth(), adminOnly)
	site.GET("/admin/registries", settingsHandler.Registries, auth.RequireAuth(), adminOnly)
	site.GET("/admin/tls", settingsHandler.TLS, auth.RequireAuth(), adminOnly)
	site.GET("/admin/audit", settingsHandler.Audit, auth.RequireAuth(), adminOnly)
	site.GET("/admin/maintenance", settingsHandler.Maintenance, auth.RequireAuth(), adminOnly)
	site.GET("/admin/update", settingsHandler.Update, auth.RequireAuth(), adminOnly)
	site.POST("/admin/update", settingsHandler.RunUpdate, auth.RequireAuth(), adminOnly)
	site.GET("/admin/update/check", settingsHandler.UpdateCheck, auth.RequireAuth(), adminOnly)
	site.GET("/admin/update/badge", settingsHandler.UpdateBadge, auth.RequireAuth(), adminOnly)
	site.GET("/admin/backups", settingsHandler.Backups, auth.RequireAuth(), adminOnly)
	site.POST("/admin/backups/destinations", settingsHandler.CreateDestination, auth.RequireAuth(), adminOnly)
	site.POST("/admin/backups/destinations/:destID/delete", settingsHandler.DeleteDestination, auth.RequireAuth(), adminOnly)
	site.POST("/admin/backups/panel", settingsHandler.SavePanelBackup, auth.RequireAuth(), adminOnly)
	site.POST("/admin/backups/panel/run", settingsHandler.RunPanelBackup, auth.RequireAuth(), adminOnly)
	site.POST("/admin/backups/panel/delete", settingsHandler.DeletePanelBackup, auth.RequireAuth(), adminOnly)
	site.POST("/admin/registry/domain", settingsHandler.SetRegistryDomain, auth.RequireAuth(), adminOnly)
	site.POST("/admin/registries", settingsHandler.CreateRegistry, auth.RequireAuth(), adminOnly)
	site.POST("/admin/registries/:id/delete", settingsHandler.DeleteRegistry, auth.RequireAuth(), adminOnly)
	site.POST("/admin/cleanup", settingsHandler.ToggleCleanup, auth.RequireAuth(), adminOnly)
	site.POST("/admin/backups/destinations/:destID/shared", settingsHandler.ToggleDestinationShared, auth.RequireAuth(), adminOnly)
	site.POST("/admin/imagewatch", settingsHandler.SaveImageWatch, auth.RequireAuth(), adminOnly)
	site.POST("/admin/dns", settingsHandler.SaveDNS, auth.RequireAuth(), adminOnly)
	site.GET("/admin/proxy", settingsHandler.ProxyPage, auth.RequireAuth(), adminOnly)
	site.POST("/admin/proxy/override", settingsHandler.SaveProxyOverride, auth.RequireAuth(), adminOnly)
	site.POST("/admin/proxy/entry", settingsHandler.SaveProxyEntry, auth.RequireAuth(), adminOnly)
	site.POST("/admin/proxy/entry/delete", settingsHandler.DeleteProxyEntry, auth.RequireAuth(), adminOnly)
	site.POST("/admin/users/:id/toggle", settingsHandler.ToggleUserActive, auth.RequireAuth(), adminOnly)
	site.POST("/admin/users/:id/admin", settingsHandler.ToggleUserAdmin, auth.RequireAuth(), adminOnly)
	// Connectors are org-scoped: the pages live on the org, the handlers ship
	// with the admin package because that is where they grew up.
	// Outside orgSettings on purpose: the wizard's repo picker calls both while
	// setup is still open. Owner-only and scoped to this org's own connectors.
	site.GET("/orgs/:slug/settings/connectors/:connectorID/branches", orgHandler.ConnectorBranches, auth.RequireAuth())
	site.GET("/orgs/:slug/settings/connectors/:connectorID/file", orgHandler.ConnectorFileExists, auth.RequireAuth())
	// GitHub is told this callback path when the app manifest is created, so it
	// stays put even though the rest of /settings moved.
	site.GET("/settings/github/callback", settingsHandler.GitHubCallback, auth.RequireAuth())
	// Old bookmarks: admins to the admin area, everyone else to their account,
	// which is where the only part they could use (API keys) now lives.
	site.GET("/settings", settingsHandler.LegacyRedirect, auth.RequireAuth())

	accountHandler := accountpage.NewHandler(deps.Store, deps.AuthService, deps.FileStorage)
	// Traefik's forwardAuth target for protected preview domains
	// (proxy.authCheckPath). Deliberately not behind RequireAuth: its whole
	// job is to answer "is there a session", and the redirect it sends has to
	// be absolute, the request arrived at a preview hostname where the
	// panel's own /login does not exist.
	site.GET("/_stackr/authcheck", authCheck(deps.Store, deps.BaseURL))

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

	notificationHandler := notificationpage.NewHandler(deps.Store)
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

	containerHandler := containerpage.NewHandler(deps.Cluster, deps.Notifier)
	site.GET("/containers", containerHandler.List, auth.RequireAuth(), adminOnly)
	site.GET("/containers/node/:id", containerHandler.NodeList, auth.RequireAuth(), adminOnly)
	site.GET("/containers/:id", containerHandler.Detail, auth.RequireAuth(), adminOnly)
	site.POST("/containers/:id/start", containerHandler.Start, auth.RequireAuth(), adminOnly)
	site.POST("/containers/:id/stop", containerHandler.Stop, auth.RequireAuth(), adminOnly)
	site.POST("/containers/:id/remove", containerHandler.Remove, auth.RequireAuth(), adminOnly)
	site.GET("/containers/:id/logs", containerHandler.LogsPage, auth.RequireAuth(), adminOnly)
	site.GET("/containers/:id/logs/stream", containerHandler.LogsStream, auth.RequireAuth(), adminOnly)
	site.GET("/containers/:id/term", containerHandler.TermPage, auth.RequireAuth(), adminOnly)
	site.GET("/containers/:id/term/ws", containerHandler.TermWS, auth.RequireAuth(), adminOnly)

	appHandler := apppage.NewHandler(deps.Store, deps.Cluster, deps.Proxy, deps.Engine, deps.Jobs, deps.GitHub, deps.Notifier)
	site.GET("/apps/:id", redirectTile(deps.Store), auth.RequireAuth())
	site.GET("/apps/:id/branches", appHandler.Branches, auth.RequireAuth())
	site.GET("/apps/:id/connectors", appHandler.Connectors, auth.RequireAuth())
	site.GET("/apps/:id/repos", appHandler.Repos, auth.RequireAuth())
	site.GET("/apps/:id/panel", appHandler.Panel, auth.RequireAuth())
	site.GET("/apps/:id/panel/content", appHandler.PanelContent, auth.RequireAuth())
	site.GET("/apps/:id/panel/header", appHandler.PanelHeader, auth.RequireAuth())
	site.GET("/apps/:id/metrics", appHandler.Metrics, auth.RequireAuth())
	site.GET("/apps/:id/logs/stream", appHandler.LogsStream, auth.RequireAuth())
	site.GET("/apps/:id/vars", appHandler.Vars, auth.RequireAuth())
	site.GET("/apps/:id/vars/value", appHandler.VarValue, auth.RequireAuth())
	site.POST("/apps/:id/vars/secret", appHandler.SaveSecretVar, auth.RequireAuth())
	site.POST("/apps/:id/vars/delete", appHandler.DeleteVar, auth.RequireAuth())
	site.POST("/apps/:id/env", appHandler.SaveEnv, auth.RequireAuth())
	site.POST("/apps/:id/settings", appHandler.SaveSettings, auth.RequireAuth())
	site.POST("/apps/:id/attach", appHandler.Attach, auth.RequireAuth())
	site.GET("/apps/:id/volume/files", appHandler.VolumeFiles, auth.RequireAuth())
	site.GET("/apps/:id/volume/files/download", appHandler.VolumeFileDownload, auth.RequireAuth())
	site.POST("/apps/:id/volume/files/upload", appHandler.VolumeFileUpload, auth.RequireAuth())
	site.POST("/apps/:id/volume/files/delete", appHandler.VolumeFileDelete, auth.RequireAuth())
	site.GET("/apps/:id/provisions", appHandler.Provisions, auth.RequireAuth())
	site.GET("/apps/:id/size", appHandler.Size, auth.RequireAuth())
	site.GET("/apps/:id/storage", appHandler.StorageFrag, auth.RequireAuth())
	site.POST("/apps/:id/storage", appHandler.AttachStorage, auth.RequireAuth())
	site.POST("/apps/:id/storage/detach", appHandler.DetachStorage, auth.RequireAuth())
	site.POST("/apps/:id/provision", appHandler.Provision, auth.RequireAuth())
	site.POST("/apps/:id/provisions/attach", appHandler.AttachProvision, auth.RequireAuth())
	site.POST("/apps/:id/provisions/:pid/detach", appHandler.DetachProvision, auth.RequireAuth())
	site.POST("/apps/:id/delete", appHandler.Delete, auth.RequireAuth())
	site.POST("/apps/:id/domains", appHandler.CreateDomain, auth.RequireAuth())
	site.POST("/apps/:id/domains/auto", appHandler.CreateAutoDomain, auth.RequireAuth())
	site.POST("/apps/:id/domains/:domainID/https", appHandler.ToggleDomainHTTPS, auth.RequireAuth())
	site.POST("/apps/:id/domains/:domainID/cert", appHandler.SetDomainCert, auth.RequireAuth())
	site.POST("/apps/:id/domains/:domainID/delete", appHandler.DeleteDomain, auth.RequireAuth())
	site.POST("/apps/:id/stop", appHandler.Stop, auth.RequireAuth())
	site.POST("/apps/:id/restart", appHandler.Restart, auth.RequireAuth())
	site.POST("/apps/:id/run", appHandler.RunNow, auth.RequireAuth())
	site.POST("/apps/:id/runs/:run/stop", appHandler.StopRun, auth.RequireAuth())
	site.GET("/apps/:id/runs/logs/stream", appHandler.RunsLogsStream, auth.RequireAuth())
	site.POST("/apps/:id/cron/toggle", appHandler.ToggleCron, auth.RequireAuth())

	deploymentHandler := deploymentpage.NewHandler(deps.Store, deps.Engine, deps.StreamHub)
	site.POST("/apps/:id/deploy", deploymentHandler.Deploy, auth.RequireAuth())
	site.POST("/apps/:id/rollback", deploymentHandler.Rollback, auth.RequireAuth())
	site.GET("/deployments/:id", deploymentHandler.Detail, auth.RequireAuth())
	site.GET("/deployments/:id/status", deploymentHandler.Status, auth.RequireAuth())
	site.GET("/deployments/:id/stream", deploymentHandler.Stream, auth.RequireAuth())
	site.POST("/deployments/:id/cancel", deploymentHandler.Cancel, auth.RequireAuth())

	// Webhooks: outside the site group, token/signature-authenticated, no CSRF/session.
	prHandler := prhook.NewHandler(deps.Store, deps.Engine, deps.Jobs,
		envops.Ops{Store: deps.Store, RT: deps.Runtime, Cluster: deps.Cluster, PX: deps.Proxy, DBs: deps.Databases}, applier, deps.GitHub, deps.Notifier).
		WithWork(deps.Work)
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
	inviteHandler := invite.NewHandler(deps.Store, deps.AuthService, deps.SessionManager)
	site.GET("/invite/:token", inviteHandler.Page)
	site.POST("/invite/:token", inviteHandler.Submit, authLimit)

	// One-time secret links. No auth either way, the token is the credential,
	// and the person on the other end has no account by design. The rate limit
	// is per-IP: the token itself is unguessable and passphrase attempts are
	// capped per link, so this only exists to blunt scripted hammering.
	shareHandler := sharepub.NewHandler(deps.Store)
	shareLimit := hamrmw.RateLimitWithConfig(hamrmw.RateLimitConfig{
		Store:  hamrmw.NewMemoryStore(hamrmw.WithMaxSize(10000)),
		Rate:   30,
		Window: time.Minute,
	})
	site.GET("/s/:token", shareHandler.Page, shareLimit)
	site.POST("/s/:token", shareHandler.Submit, shareLimit)

	// Canonical slug URLs: /:org/:stack[/:env[/:tile]]. Registered last;
	// echo prefers static segments, so reserved top-level paths always win.
	site.GET("/:org/:stack", projectHandler.StackGraph, auth.RequireAuth())
	// Stack settings: one URL per section, plus a page per environment. These
	// were all one long scroll, with environment settings hidden in disclosures.
	site.GET("/:org/:stack/plans/:planID", projectHandler.PlanView, auth.RequireAuth())
	site.GET("/:org/:stack/releases", projectHandler.Releases, auth.RequireAuth())
	site.GET("/:org/:stack/settings", projectHandler.Settings, auth.RequireAuth())
	site.GET("/:org/:stack/settings/general", projectHandler.SettingsGeneral, auth.RequireAuth())
	site.GET("/:org/:stack/settings/config", projectHandler.SettingsConfig, auth.RequireAuth())
	site.GET("/:org/:stack/settings/config/export", projectHandler.ExportConfig, auth.RequireAuth())
	site.GET("/:org/:stack/settings/variables", projectHandler.SettingsVariables, auth.RequireAuth())
	site.GET("/:org/:stack/settings/variables/panel", projectHandler.StackVarsPanel, auth.RequireAuth())
	site.GET("/:org/:stack/settings/variables/value", projectHandler.StackVarValue, auth.RequireAuth())
	site.GET("/:org/:stack/settings/domains", projectHandler.SettingsDomains, auth.RequireAuth())
	site.GET("/:org/:stack/settings/environments", projectHandler.SettingsEnvironments, auth.RequireAuth())
	site.GET("/:org/:stack/settings/environments/:env", projectHandler.SettingsEnvironment, auth.RequireAuth())
	site.GET("/:org/:stack/settings/environments/:env/variables/panel", projectHandler.EnvVarsPanel, auth.RequireAuth())
	site.GET("/:org/:stack/settings/environments/:env/variables/value", projectHandler.EnvVarValue, auth.RequireAuth())
	site.GET("/:org/:stack/settings/pr", projectHandler.SettingsPREnv, auth.RequireAuth())
	site.GET("/:org/:stack/:env", projectHandler.Graph, auth.RequireAuth())
	site.GET("/:org/:stack/:env/logs", projectHandler.EnvLogs, auth.RequireAuth())
	site.GET("/:org/:stack/:env/:tile", tilePage(deps.Store, appHandler.Detail, dbHandler.Detail), auth.RequireAuth())
}

// authCheck answers Traefik's forwardAuth for a protected preview domain.
//
// "Is there a session" is not the question. Preview hostnames belong to a
// tile, that tile to an org, and a session proves only that someone works
// here, answering 204 on a session alone lets any user of the panel browse
// every other organization's preview URLs. Traefik forwards the hostname it
// was asked for, so the owning org is knowable, and the check is the same one
// the rest of the panel makes.
//
// Deliberately not behind RequireAuth: its job includes answering for signed
// -out visitors, and the redirect must be absolute, the request arrived at a
// preview hostname where the panel's own /login does not exist.
func authCheck(store repo.Store, baseURL string) echo.HandlerFunc {
	login := strings.TrimRight(baseURL, "/") + "/login"
	return func(c echo.Context) error {
		if hamrmw.GetSubjectID(c) == "" {
			return c.Redirect(http.StatusSeeOther, login)
		}
		host := c.Request().Header.Get("X-Forwarded-Host")
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		ctx := c.Request().Context()
		// Auto domains are always created at "/" (envops.EnsureAutoDomain), and
		// this middleware is only attached to them, so an exact lookup is enough.
		// An unknown host is not a domain stackr routes: refuse rather than
		// wave it through on the strength of a session.
		dom, err := store.GetDomainByHostPath(ctx, host, "/")
		if err != nil || dom == nil {
			return echo.NewHTTPError(http.StatusForbidden, "not your preview URL")
		}
		t, err := store.GetTile(ctx, dom.TileID)
		if err != nil || t == nil {
			return echo.NewHTTPError(http.StatusForbidden, "not your preview URL")
		}
		s, err := store.GetStack(ctx, t.StackID)
		if err != nil || s == nil {
			return echo.NewHTTPError(http.StatusForbidden, "not your preview URL")
		}
		if !middleware.InOrg(c, s.OrgID) {
			// Logged in, wrong tenant: another trip through /login changes
			// nothing, so this is a refusal and not a redirect.
			return echo.NewHTTPError(http.StatusForbidden, "not your preview URL")
		}
		return c.NoContent(http.StatusNoContent)
	}
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
