// Package v1 is the REST API. Every operation is declared through op(), which
// registers the Echo route AND its OpenAPI entry from the same Go types, the
// spec at /api/openapi.json cannot drift from the handlers.
//
// The package is split by resource: stacks.go, apps.go, logs.go,
// databases.go hold the handlers; types.go the request/response shapes;
// helpers.go the shared plumbing; auth.go/errors.go the middleware. This file
// is just the machinery, the API struct, op(), the route table, and the spec.
package v1

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/swaggest/openapi-go"
	"github.com/swaggest/openapi-go/openapi3"

	"github.com/FyrmForge/stackr/internal/stackrd/config/orgconf"
	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/forward"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	svcmail "github.com/FyrmForge/stackr/internal/stackrd/service/mail"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	svcproxy "github.com/FyrmForge/stackr/internal/stackrd/service/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type API struct {
	store   repo.Store
	engine  *deploy.Engine
	rt      *runtime.Runtime
	clus    *cluster.Cluster
	work    *workqueue.Queue
	px      *svcproxy.Service
	jobs    *jobs.Service
	backups *backup.Service   // nil in tests
	fwd     *forward.Registry // live tunnels, for the canvas cards; nil in tests
	notify  *notify.Notifier  // pushes open/close to stack rooms; nil in tests
	// applier runs config-as-code plans. The same value the web canvas uses, so
	// a plan approved over the API applies exactly as one approved in the panel.
	// Zero value in tests that don't touch /config.
	applier stackconf.Applier
	// signer mints the managed registry's tokens. The registry answers only
	// bearer tokens, so the catalog reads behind /registry/images need it.
	signer *registry.Signer
	// sched re-registers the cron and backup tables after anything that
	// cascades their rows. Nil in tests; its methods are nil-safe.
	sched *scheduler.Service
	// mail sends the invite. Nil in tests and wherever no provider is set.
	mail *svcmail.Service
	// life owns stop/restart/pause/run; tiles owns the row and its teardown.
	// Nil in tests that never call them.
	life  *service.TileLifecycleService
	tiles *service.TileService
	// telemetry resolves where a tile's logs and metrics come from.
	telemetry *service.TileTelemetryService
	// domains owns the hostnames a tile answers on; resources owns the
	// hostnames stackr may generate names under.
	domains   *service.DomainService
	resources *service.DomainResourceService
	// instances owns a managed database instance; slices owns what is cut
	// out of one.
	instances *service.ManagedInstanceService
	slices    *service.SliceService
	// vars owns every write to the variables table and what it earns;
	// envs owns the environment row and its cascade.
	vars *service.VariableService
	envs *service.EnvironmentService
	// stacks owns the stack row: create, rename, delete, move, bind.
	stacks *service.StackService
	// deploys owns which tiles may be deployed; releases owns the ladder.
	deploys  *service.DeployService
	releases *service.ReleaseService
	// plans owns a config plan's life after it exists: replan, approve,
	// reject.
	plans *service.PlanService
	// schedules owns a tile's backup schedules; dests owns the buckets they
	// are written to.
	schedules *service.BackupScheduleService
	dests     *service.BackupDestinationService
	// storage owns a share or pool and its sub-paths.
	storage *service.StorageService
	// registries owns the registry rows, the org credentials and the tags.
	registries *service.RegistryService
	// prenvs owns a stack's pull-request environment settings.
	prenvs *service.PREnvService
	// orgcfg is the org config-as-code runner, wired once in main.
	orgcfg *orgconf.Runner
	// members owns who is in an org and at what level.
	members *service.MemberService
	// orgs owns the organization row and the setup draft.
	orgs *service.OrgService
	// settings owns every rung of the defaults cascade.
	settings *service.SettingsService
	// access owns the level every verb needs, shared with the panel.
	access *service.AccessService
	spec   *openapi3.Reflector
}

// securityName is the OpenAPI security scheme id for the x-api-key header.
const securityName = "apiKey"

func New(store repo.Store, engine *deploy.Engine, rt *runtime.Runtime, clus *cluster.Cluster, px *svcproxy.Service, js *jobs.Service, bk *backup.Service, fwd *forward.Registry, n *notify.Notifier, ap stackconf.Applier) *API {
	r := &openapi3.Reflector{}
	r.Spec = &openapi3.Spec{Openapi: "3.0.3"}
	r.Spec.Info.WithTitle("Stackr API").WithVersion("1.0.0")
	r.SpecEns().SetAPIKeySecurity(securityName, "x-api-key", openapi.InHeader, "API key")
	return &API{store: store, engine: engine, rt: rt, clus: clus, px: px, jobs: js, backups: bk, fwd: fwd, notify: n, applier: ap, spec: r}
}

// WithWork gives the API the durable job runner. An approve enqueues an apply
// and answers 202: a CI job holding the connection open is not what keeps an
// apply alive, the queue is.
func (a *API) WithWork(q *workqueue.Queue) *API { a.work = q; return a }

// WithRegistrySigner gives the API the managed registry's token signer. Nil
// when the keypair would not load, which the registry routes report rather
// than failing at the registry with a bare 401.
func (a *API) WithRegistrySigner(s *registry.Signer) *API { a.signer = s; return a }

// WithScheduler gives the API the schedule reloader.
func (a *API) WithScheduler(s *scheduler.Service) *API { a.sched = s; return a }

// WithLifecycle gives the API the tile lifecycle service, the same value the
// panel's handlers hold, so a script and a person get the same side effects.
func (a *API) WithLifecycle(l *service.TileLifecycleService) *API { a.life = l; return a }

// WithTiles gives the API the tile service, the same value the panel holds.
func (a *API) WithTiles(t *service.TileService) *API { a.tiles = t; return a }

// WithTelemetry gives the API the log and metric resolvers the panel uses.
func (a *API) WithTelemetry(t *service.TileTelemetryService) *API { a.telemetry = t; return a }

// WithDomains gives the API the domain service the panel uses.
func (a *API) WithDomains(d *service.DomainService) *API { a.domains = d; return a }

// WithInstances gives the API the managed-instance service.
func (a *API) WithInstances(m *service.ManagedInstanceService) *API { a.instances = m; return a }

// WithSlices gives the API the slice service.
func (a *API) WithSlices(sl *service.SliceService) *API { a.slices = sl; return a }

// WithVariables gives the API the variable service.
func (a *API) WithVariables(v *service.VariableService) *API { a.vars = v; return a }

// WithEnvironments gives the API the environment service.
func (a *API) WithEnvironments(e *service.EnvironmentService) *API { a.envs = e; return a }

// WithStacks gives the API the stack service.
func (a *API) WithStacks(st *service.StackService) *API { a.stacks = st; return a }

// WithAccess gives the API the access service.
func (a *API) WithAccess(ac *service.AccessService) *API { a.access = ac; return a }

// WithSettings gives the API the settings service.
func (a *API) WithSettings(st *service.SettingsService) *API { a.settings = st; return a }

// WithDeploys gives the API the deploy service.
func (a *API) WithDeploys(d *service.DeployService) *API { a.deploys = d; return a }

// WithReleases gives the API the release service.
func (a *API) WithReleases(r *service.ReleaseService) *API { a.releases = r; return a }

// WithPlans gives the API the plan service.
func (a *API) WithPlans(p *service.PlanService) *API { a.plans = p; return a }

// WithBackupServices gives the API the schedule and destination services.
func (a *API) WithBackupServices(sch *service.BackupScheduleService, d *service.BackupDestinationService) *API {
	a.schedules, a.dests = sch, d
	return a
}

// WithStorage gives the API the storage service.
func (a *API) WithStorage(st *service.StorageService) *API { a.storage = st; return a }

// WithRegistries gives the API the registry service.
func (a *API) WithRegistries(r *service.RegistryService) *API { a.registries = r; return a }

// WithPREnvs gives the API the pull-request settings service.
func (a *API) WithPREnvs(p *service.PREnvService) *API { a.prenvs = p; return a }

// WithMembers gives the API the membership service.
func (a *API) WithMembers(m *service.MemberService) *API { a.members = m; return a }

// WithOrgs gives the API the organization service.
func (a *API) WithOrgs(o *service.OrgService) *API { a.orgs = o; return a }

// WithOrgConfig gives the API the org config runner.
func (a *API) WithOrgConfig(r *orgconf.Runner) *API { a.orgcfg = r; return a }

// WithDomainResources gives the API the domain-resource service.
func (a *API) WithDomainResources(r *service.DomainResourceService) *API { a.resources = r; return a }

// WithMail gives the API the outgoing mailer. Nil sends nothing, which is what
// this path did unconditionally before.
func (a *API) WithMail(m *svcmail.Service) *API { a.mail = m; return a }

// op registers a route (guarded by its required scope) and mirrors it into the
// OpenAPI spec, the route, its scope, and its spec entry all come from one
// call, so the published spec can't drift from what's enforced.
//
// desc is an optional longer note for the spec. It is variadic so the summary
// stays the short name every operation id is derived from: a summary long
// enough to explain a side effect would rename the generated client method.
func op[Req, Resp any](a *API, g *echo.Group, method, path, scope, summary string, h echo.HandlerFunc, desc ...string) {
	g.Add(method, path, a.requireScope(scope, h))
	a.spec1(method, path, scope, summary, opTypes[Req, Resp]{}, desc...)
}

// opw registers a MUTATING route. It is op plus the two things a write owes:
// the verb it performs and the kind of thing it addresses, which together are
// point 18's authorization — resolved and checked before the handler runs.
//
// Separate from op rather than three more arguments on it, so that "this
// route writes and here is what it may do" is visible at the call site, and
// so a mutating route registered with op instead fails the route-verb test
// rather than quietly registering with no verb.
//
// param names the path parameter carrying k's reference. The scope is
// unchanged and orthogonal: it narrows an API key below its user's rights,
// while the verb is the user's own standing, and requireScope still runs.
func opw[Req, Resp any](a *API, g *echo.Group, method, path, scope string,
	v service.Verb, k service.Kind, param, summary string, h echo.HandlerFunc, desc ...string) {
	g.Add(method, path, a.requireScope(scope, a.gate(v, k, param, h))).Name = string(v)
	a.spec1(method, path, scope, summary, opTypes[Req, Resp]{}, desc...)
}

// opr registers a gated READ. Same bargain as opw — the gate is mounted and
// the verb recorded from one call — and the same arguments, minus the method,
// which is always GET.
//
// op survives for the reads a gate cannot answer: a collection endpoint spans
// orgs, so there is no single tenancy to resolve and the filter lives in the
// handler, per row. routeread_test.go lists which ones and why.
func opr[Req, Resp any](a *API, g *echo.Group, path, scope string,
	v service.Verb, k service.Kind, param, summary string, h echo.HandlerFunc, desc ...string) {
	g.Add(http.MethodGet, path, a.requireScope(scope, a.gate(v, k, param, h))).Name = string(v)
	a.spec1(http.MethodGet, path, scope, summary, opTypes[Req, Resp]{}, desc...)
}

// opTypes carries op's two type parameters into the shared spec registration.
type opTypes[Req, Resp any] struct{}

func (opTypes[Req, Resp]) req() any  { var r Req; return r }
func (opTypes[Req, Resp]) resp() any { var r Resp; return r }

type specTypes interface {
	req() any
	resp() any
}

func (a *API) spec1(method, path, scope, summary string, t specTypes, desc ...string) {
	oc, err := a.spec.NewOperationContext(method, "/api/v1"+specPath(path))
	if err != nil {
		panic("openapi: " + method + " " + path + ": " + err.Error())
	}
	oc.SetID(operationID(summary)) // derive the codegen method name before the scope suffix
	if scope != "" {
		summary += " (scope: " + scope + ")"
	}
	oc.SetSummary(summary)
	if len(desc) > 0 {
		oc.SetDescription(strings.Join(desc, " "))
	}
	oc.AddSecurity(securityName)
	oc.AddReqStructure(t.req())
	oc.AddRespStructure(t.resp(), withStatus(successStatus(method, path)))
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		oc.AddRespStructure(errBody{}, withStatus(code))
	}
	// Loudly: a spec error means the published document no longer describes the
	// route that was just registered, and a silently skipped operation is a
	// missing client method nobody notices until a user hits it.
	if err := a.spec.AddOperation(oc); err != nil {
		panic("openapi: " + method + " " + path + ": " + err.Error())
	}
}

// specPath turns an Echo path into an OpenAPI one: every ":param" becomes
// "{param}". It used to be a fixed replacer of three names, so a route with any
// other parameter published a literal ":run" in the spec and generated a broken
// client.
func specPath(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, ":") {
			segs[i] = "{" + s[1:] + "}"
		}
	}
	return strings.Join(segs, "/")
}

func withStatus(code int) openapi.ContentOption {
	return func(cu *openapi.ContentUnit) { cu.HTTPStatus = code }
}

// successStatus mirrors what the handlers actually return, so the spec doesn't
// claim 200 for every route.
func successStatus(method, path string) int {
	switch {
	case strings.HasSuffix(path, "/deploy"):
		return http.StatusAccepted
	// A decision on an existing plan, not a new resource.
	case strings.HasSuffix(path, "/approve"), strings.HasSuffix(path, "/reject"):
		return http.StatusOK
	// A computation, not a creation, nothing is stored.
	case strings.HasSuffix(path, "/plan-preview"):
		return http.StatusOK
	case method == http.MethodPost:
		return http.StatusCreated
	case method == http.MethodDelete:
		return http.StatusNoContent
	default:
		return http.StatusOK
	}
}

// operationID turns a human summary ("Get app") into a codegen-friendly method
// name ("getApp"). Summaries are unique, so the ids are too.
func operationID(summary string) string {
	var b strings.Builder
	for i, w := range strings.Fields(summary) {
		w = strings.ToLower(w)
		if i == 0 {
			b.WriteString(w)
			continue
		}
		b.WriteString(strings.ToUpper(w[:1]) + w[1:])
	}
	return b.String()
}

// Register wires all v1 routes onto the group (which must carry KeyAuth). Each
// op declares the capability scope it requires.
func (a *API) Register(g *echo.Group) {
	// the defaults cascade: server -> org -> stack -> env. One shape at every
	// level, so a caller reads the same rows wherever it looks.
	opr[struct{}, settingsOut](a, g, "/settings", ScopeStacksRead, service.VerbAdminRead, service.KindNone, "", "Get server defaults", a.settingsFor("server"))
	opw[map[string]*string, settingsOut](a, g, http.MethodPatch, "/settings", ScopeStacksWrite, service.VerbServerDefaults, service.KindNone, "", "Set server defaults", a.patchSettingsFor("server"))
	opr[idParam, settingsOut](a, g, "/orgs/:id/settings", ScopeStacksRead, service.VerbOrgRead, service.KindOrg, "id", "Get organization defaults", a.settingsFor("org"))
	opw[idParam, settingsOut](a, g, http.MethodPatch, "/orgs/:id/settings", ScopeStacksWrite, service.VerbOrgDefaults, service.KindOrg, "id", "Set organization defaults", a.patchSettingsFor("org"))
	opr[idParam, settingsOut](a, g, "/stacks/:id/settings", ScopeStacksRead, service.VerbOrgRead, service.KindStack, "id", "Get stack defaults", a.settingsFor("stack"))
	opw[idParam, settingsOut](a, g, http.MethodPatch, "/stacks/:id/settings", ScopeStacksWrite, service.VerbOrgDefaults, service.KindStack, "id", "Set stack defaults", a.patchSettingsFor("stack"))
	opr[idParam, settingsOut](a, g, "/envs/:id/settings", ScopeStacksRead, service.VerbOrgRead, service.KindEnv, "id", "Get environment defaults", a.settingsFor("env"))
	opw[idParam, settingsOut](a, g, http.MethodPatch, "/envs/:id/settings", ScopeEnvsWrite, service.VerbOrgDefaults, service.KindEnv, "id", "Set environment defaults", a.patchSettingsFor("env"))
	// orgs: the slug every path form starts with, so this is read-only and
	// carries the caller's own role rather than the org's members.
	op[struct{}, []orgOut](a, g, http.MethodGet, "/orgs", ScopeStacksRead, "List organizations", a.listOrgs)
	// registries: the managed one's TLS domain and the external ones a tile can
	// pull from. Server-wide, so admin-only.
	opr[struct{}, []registryOut](a, g, "/registries", ScopeStacksRead, service.VerbAdminRead, service.KindNone, "", "List registries", a.adminOnly(a.listRegistries))
	opw[registryIn, registryOut](a, g, http.MethodPost, "/registries", ScopeStacksWrite, service.VerbServerDefaults, service.KindNone, "", "Add an external registry", a.adminOnly(a.createRegistry))
	opw[registryPatch, registryOut](a, g, http.MethodPatch, "/registries/:id", ScopeStacksWrite, service.VerbServerDefaults, service.KindNone, "", "Update a registry", a.adminOnly(a.patchRegistry))
	opw[idParam, struct{}](a, g, http.MethodDelete, "/registries/:id", ScopeStacksWrite, service.VerbServerDefaults, service.KindNone, "", "Remove an external registry", a.adminOnly(a.deleteRegistry))
	// members, roles and invites: onboarding a person was panel-only, so it
	// was the one thing nobody could script.
	opr[idParam, []memberOut](a, g, "/orgs/:id/members", ScopeStacksRead, service.VerbMemberList, service.KindOrg, "id", "List organization members", a.listMembers)
	opw[inviteIn, inviteOut](a, g, http.MethodPost, "/orgs/:id/members", ScopeStacksWrite, service.VerbMemberManage, service.KindOrg, "id", "Invite a member", a.addMember)
	opw[roleIn, memberOut](a, g, http.MethodPatch, "/orgs/:id/members/:user", ScopeStacksWrite, service.VerbMemberManage, service.KindOrg, "id", "Change a member's role", a.setMemberRole)
	opw[roleIn, struct{}](a, g, http.MethodDelete, "/orgs/:id/members/:user", ScopeStacksWrite, service.VerbMemberManage, service.KindOrg, "id", "Remove a member", a.removeMember)
	opr[idParam, []inviteOut](a, g, "/orgs/:id/invites", ScopeStacksRead, service.VerbOrgOwnerRead, service.KindOrg, "id", "List open invites", a.listInvites)
	opw[inviteIn, inviteOut](a, g, http.MethodPost, "/orgs/:id/invites", ScopeStacksWrite, service.VerbMemberManage, service.KindOrg, "id", "Create an invite", a.createInvite)
	opw[inviteParam, struct{}](a, g, http.MethodDelete, "/orgs/:id/invites/:invite", ScopeStacksWrite, service.VerbMemberManage, service.KindOrg, "id", "Revoke an invite", a.deleteInvite)
	// org registry: push credentials and the images they produced. Reads are
	// filtered to the caller's namespace here; the registry enforces the same
	// boundary on the wire through scoped tokens.
	opr[idParam, []registryCredentialOut](a, g, "/orgs/:id/registry/credentials", ScopeStacksRead, service.VerbOrgRead, service.KindOrg, "id", "List registry credentials", a.listRegistryCredentials)
	opw[registryCredentialIn, registryCredentialOut](a, g, http.MethodPost, "/orgs/:id/registry/credentials", ScopeStacksWrite, service.VerbRegistryCredential, service.KindOrg, "id", "Create a registry credential", a.createRegistryCredential)
	opw[registryCredParam, struct{}](a, g, http.MethodDelete, "/orgs/:id/registry/credentials/:cred", ScopeStacksWrite, service.VerbRegistryCredential, service.KindOrg, "id", "Delete a registry credential", a.deleteRegistryCredential)
	opr[idParam, []registryImageOut](a, g, "/orgs/:id/registry/images", ScopeStacksRead, service.VerbRegistryList, service.KindOrg, "id", "List registry images", a.listRegistryImages)
	opr[registryImageParam, []registryTagOut](a, g, "/orgs/:id/registry/images/:name/tags", ScopeStacksRead, service.VerbRegistryList, service.KindOrg, "id", "List an image's tags", a.listRegistryTags)
	opw[registryTagParam, struct{}](a, g, http.MethodDelete, "/orgs/:id/registry/images/:name/tags/:tag", ScopeStacksWrite, service.VerbRegistryTagDelete, service.KindOrg, "id", "Delete an image tag", a.deleteRegistryTag)
	// stacks
	op[struct{}, []stackOut](a, g, http.MethodGet, "/stacks", ScopeStacksRead, "List stacks", a.listStacks)
	opw[stackIn, stackOut](a, g, http.MethodPost, "/stacks", ScopeStacksWrite, service.VerbStackCreate, service.KindDeferred, "", "Create stack", a.createStack)
	opw[idParam, struct{}](a, g, http.MethodDelete, "/stacks/:id", ScopeStacksWrite, service.VerbStackWrite, service.KindStack, "id", "Delete stack", a.deleteStack)
	opr[idParam, []envOut](a, g, "/stacks/:id/envs", ScopeStacksRead, service.VerbOrgRead, service.KindStack, "id", "List environments", a.listEnvs)
	opw[envIn, envOut](a, g, http.MethodPost, "/stacks/:id/envs", ScopeEnvsWrite, service.VerbStackWrite, service.KindStack, "id", "Create environment", a.createEnv)
	// promotion: what is promotable, and moving one commit up a rung
	// environment lifecycle
	opw[forceIn, struct{}](a, g, http.MethodDelete, "/envs/:id", ScopeEnvsWrite, service.VerbEnvWrite, service.KindEnv, "id", "Delete environment", a.deleteEnv)
	opw[forceIn, struct{}](a, g, http.MethodPost, "/envs/:id/reset", ScopeEnvsWrite, service.VerbEnvWrite, service.KindEnv, "id", "Reset a config-managed environment", a.resetEnv)
	opw[copyEnvIn, envOut](a, g, http.MethodPost, "/envs/:id/copy", ScopeEnvsWrite, service.VerbEnvWrite, service.KindEnv, "id", "Copy an environment", a.copyEnv)
	opw[envPatch, envOut](a, g, http.MethodPatch, "/envs/:id", ScopeEnvsWrite, service.VerbEnvWrite, service.KindEnv, "id", "Update environment settings", a.patchEnv)
	opr[idParam, []releaseOut](a, g, "/stacks/:id/releases", ScopeAppsRead, service.VerbOrgRead, service.KindStack, "id", "List releases", a.listReleases)
	opw[promoteIn, promoteOut](a, g, http.MethodPost, "/stacks/:id/envs/:slug/promote", ScopeAppsDeploy, service.VerbStackWrite, service.KindStack, "id", "Promote a commit to an environment", a.promote)
	// apps
	op[appsQuery, []appOut](a, g, http.MethodGet, "/apps", ScopeAppsRead, "List apps", a.listApps)
	opw[appIn, appOut](a, g, http.MethodPost, "/stacks/:id/apps", ScopeAppsWrite, service.VerbStackWrite, service.KindStack, "id", "Create app", a.createApp)
	opr[idParam, appOut](a, g, "/apps/:id", ScopeAppsRead, service.VerbTileRead, service.KindTile, "id", "Get app", a.getApp)
	opw[appPatch, appOut](a, g, http.MethodPatch, "/apps/:id", ScopeAppsWrite, service.VerbTileWrite, service.KindTile, "id", "Update app", a.patchApp)
	opw[idParam, struct{}](a, g, http.MethodDelete, "/apps/:id", ScopeAppsWrite, service.VerbTileWrite, service.KindTile, "id", "Delete app", a.deleteApp)
	opw[idParam, deployAccepted](a, g, http.MethodPost, "/apps/:id/deploy", ScopeAppsDeploy, service.VerbTileWrite, service.KindTile, "id", "Trigger deployment", a.deployApp)
	opw[idParam, runAccepted](a, g, http.MethodPost, "/apps/:id/run", ScopeAppsDeploy, service.VerbTileWrite, service.KindTile, "id", "Run now (cron/function one-shot)", a.runApp)
	opw[runStopIn, runStopped](a, g, http.MethodPost, "/apps/:id/runs/:run/stop", ScopeAppsDeploy, service.VerbTileWrite, service.KindTile, "id", "Stop a run in flight", a.stopRun)
	// tile lifecycle: the panel's buttons, so a script can do what a person can
	opw[idParam, appOut](a, g, http.MethodPost, "/apps/:id/stop", ScopeAppsDeploy, service.VerbTileWrite, service.KindTile, "id", "Stop app", a.stopApp)
	opw[idParam, appOut](a, g, http.MethodPost, "/apps/:id/restart", ScopeAppsDeploy, service.VerbTileWrite, service.KindTile, "id", "Restart app", a.restartApp)
	opw[idParam, appOut](a, g, http.MethodPost, "/apps/:id/cron/toggle", ScopeAppsDeploy, service.VerbTileWrite, service.KindTile, "id", "Pause or resume a cron schedule", a.toggleCron)
	opw[rollbackIn, deployAccepted](a, g, http.MethodPost, "/apps/:id/rollback", ScopeAppsDeploy, service.VerbTileWrite, service.KindTile, "id", "Roll back to an image tag", a.rollbackApp)
	opw[idParam, deploymentOut](a, g, http.MethodPost, "/deployments/:id/cancel", ScopeAppsDeploy, service.VerbDeploymentCancel, service.KindDeployment, "id", "Cancel a deployment", a.cancelDeployment)
	opr[idParam, []metricOut](a, g, "/apps/:id/metrics", ScopeAppsRead, service.VerbTileRead, service.KindTile, "id", "Get app metrics", a.appMetrics)
	// stack settings the panel owned alone
	opw[stackPatch, stackOut](a, g, http.MethodPatch, "/stacks/:id", ScopeStacksWrite, service.VerbStackWrite, service.KindStack, "id", "Rename stack", a.patchStack,
		"The slug moves with the name, so every URL and container name under the stack moves too. Swarm cannot rename a service, so running tiles are stopped under the old name and redeployed under the new one. Volumes, databases and history are keyed by id and survive. A config-managed stack is refused: its file owns the name.")
	opr[idParam, prEnvOut](a, g, "/stacks/:id/pr-envs", ScopeStacksRead, service.VerbOrgRead, service.KindStack, "id", "Get PR environment settings", a.getPREnv)
	opw[prEnvIn, prEnvOut](a, g, http.MethodPut, "/stacks/:id/pr-envs", ScopeStacksWrite, service.VerbStackWrite, service.KindStack, "id", "Set PR environment settings", a.putPREnv)
	// org config-as-code (§6): plans only, the binding is panel-owned
	opw[idParam, planOut](a, g, http.MethodPost, "/orgs/:id/config/plan", ScopeConfigApply, service.VerbOrgConfigBind, service.KindOrg, "id", "Plan org config", a.planOrg)
	opr[idParam, []planOut](a, g, "/orgs/:id/config/plans", ScopeConfigRead, service.VerbOrgOwnerRead, service.KindOrg, "id", "List org config plans", a.listOrgPlans)
	opw[orgPreviewIn, planDetailOut](a, g, http.MethodPost, "/orgs/:id/config/plan-preview", ScopeConfigApply, service.VerbOrgConfigBind, service.KindOrg, "id", "Preview org config plan", a.previewOrgPlan)
	opr[idParam, planDetailOut](a, g, "/org-config/plans/:id", ScopeConfigRead, service.VerbOrgOwnerRead, service.KindOrgPlan, "id", "Get an org config plan", a.getOrgPlan)
	opw[idParam, planOut](a, g, http.MethodPost, "/org-config/plans/:id/approve", ScopeConfigApply, service.VerbOrgPlanApprove, service.KindOrgPlan, "id", "Approve + apply an org plan", a.approveOrgPlan)
	opw[idParam, planOut](a, g, http.MethodPost, "/org-config/plans/:id/reject", ScopeConfigApply, service.VerbOrgPlanApprove, service.KindOrgPlan, "id", "Reject an org plan", a.rejectOrgPlan)
	// storage tiles (server-wide shares/pools; attachments ride the tile PATCH)
	opr[struct{}, []storageOut](a, g, "/storage", ScopeStorageRead, service.VerbAdminRead, service.KindNone, "", "List storage", a.adminOnly(a.listStorage))
	opw[storageIn, storageOut](a, g, http.MethodPost, "/storage", ScopeStorageWrite, service.VerbNodeManage, service.KindNone, "", "Create storage (probes on create)", a.adminOnly(a.createStorage))
	opw[idParam, storageOut](a, g, http.MethodPost, "/storage/:id/probe", ScopeStorageWrite, service.VerbNodeManage, service.KindNone, "", "Probe storage", a.adminOnly(a.probeStorage))
	opw[idParam, struct{}](a, g, http.MethodDelete, "/storage/:id", ScopeStorageWrite, service.VerbNodeManage, service.KindNone, "", "Delete storage", a.adminOnly(a.deleteStorage))
	opw[storagePathIn, storagePathOut](a, g, http.MethodPost, "/storage/:id/paths", ScopeStorageWrite, service.VerbNodeManage, service.KindNone, "", "Declare a sub-path", a.adminOnly(a.createStoragePath))
	opw[idParam, struct{}](a, g, http.MethodDelete, "/storage/paths/:id", ScopeStorageWrite, service.VerbNodeManage, service.KindNone, "", "Remove a sub-path", a.adminOnly(a.deleteStoragePath))
	// proxy escape hatches (server-wide; domains:write is the closest scope)
	opr[struct{}, proxyConfigOut](a, g, "/proxy/config", ScopeDomainsWrite, service.VerbAdminRead, service.KindNone, "", "Get proxy config + escape hatches", a.adminOnly(a.getProxyConfig))
	opw[proxyOverrideIn, proxyConfigOut](a, g, http.MethodPut, "/proxy/static-override", ScopeDomainsWrite, service.VerbProxyAdmin, service.KindNone, "", "Set/clear the static traefik.yml override", a.adminOnly(a.putProxyOverride))
	opw[proxyEntryIn, proxyConfigOut](a, g, http.MethodPut, "/proxy/entries/:name", ScopeDomainsWrite, service.VerbProxyAdmin, service.KindNone, "", "Write a custom dynamic entry", a.adminOnly(a.putProxyEntry))
	opw[proxyEntryParam, struct{}](a, g, http.MethodDelete, "/proxy/entries/:name", ScopeDomainsWrite, service.VerbProxyAdmin, service.KindNone, "", "Delete a custom dynamic entry", a.adminOnly(a.deleteProxyEntry))
	// variables
	opr[idParam, varsOut](a, g, "/apps/:id/variables", ScopeVarsRead, service.VerbTileRead, service.KindTile, "id", "Get app variables", a.getVars)
	opw[varsIn, varsOut](a, g, http.MethodPut, "/apps/:id/variables", ScopeVarsWrite, service.VerbVariableWrite, service.KindTile, "id", "Replace app variables", a.putVars)
	// The stored expressions verbatim, what to edit, as opposed to what a
	// container will see. Same handler as GET /variables: the name is what
	// pairs with /resolved, and a second copy of the body would be a second
	// place for the audit rules to drift.
	opr[idParam, varsOut](a, g, "/apps/:id/variables/unresolved", ScopeVarsRead, service.VerbTileRead, service.KindTile, "id", "Get stored variable expressions", a.getVars)
	opr[idParam, varsOut](a, g, "/apps/:id/variables/resolved", ScopeVarsRead, service.VerbTileRead, service.KindTile, "id", "Get resolved variables", a.resolvedVars)
	opr[idParam, []refSourceOut](a, g, "/apps/:id/reference-catalogue", ScopeVarsRead, service.VerbTileRead, service.KindTile, "id", "List referenceable sources", a.referenceCatalogue)
	opr[idParam, varsOut](a, g, "/stacks/:id/variables", ScopeVarsRead, service.VerbOrgRead, service.KindStack, "id", "Get stack variables", a.getStackVars)
	opw[varsIn, varsOut](a, g, http.MethodPut, "/stacks/:id/variables", ScopeVarsWrite, service.VerbVariableWrite, service.KindStack, "id", "Replace stack variables", a.putStackVars)
	opr[idParam, varsOut](a, g, "/envs/:id/variables", ScopeVarsRead, service.VerbOrgRead, service.KindEnv, "id", "Get environment variables", a.getEnvVars)
	opw[varsIn, varsOut](a, g, http.MethodPut, "/envs/:id/variables", ScopeVarsWrite, service.VerbVariableWrite, service.KindEnv, "id", "Replace environment variables", a.putEnvVars)
	opr[idParam, varsOut](a, g, "/orgs/:id/variables", ScopeVarsRead, service.VerbOrgRead, service.KindOrg, "id", "Get org variables", a.getOrgVars)
	opw[varsIn, varsOut](a, g, http.MethodPut, "/orgs/:id/variables", ScopeVarsWrite, service.VerbVariableWrite, service.KindOrg, "id", "Replace org variables", a.putOrgVars)
	opr[idParam, []resourceOut](a, g, "/apps/:id/resources", ScopeDBsRead, service.VerbTileRead, service.KindTile, "id", "List app managed resources", a.listAppResources)
	// config-as-code: plans are proposed by a push (or by planning now) and
	// applied by approval, which is what a gated environment's manual policy
	// waits for.
	opr[plansQuery, []planOut](a, g, "/stacks/:id/config/plans", ScopeConfigRead, service.VerbOrgRead, service.KindStack, "id", "List config plans", a.listPlans)
	opw[idParam, []planDetailOut](a, g, http.MethodPost, "/stacks/:id/config/plan", ScopeConfigApply, service.VerbStackPlan, service.KindStack, "id", "Create config plan", a.planStack)
	opw[previewIn, planDetailOut](a, g, http.MethodPost, "/stacks/:id/config/plan-preview", ScopeConfigApply, service.VerbStackPlan, service.KindStack, "id", "Preview config plan", a.previewPlan)
	// export: live state as the file that would produce it, so a panel-first
	// stack can move to config-as-code without hand-writing one.
	opr[idParam, struct{}](a, g, "/stacks/:id/config/export", ScopeConfigRead, service.VerbConfigExport, service.KindStack, "id", "Export the stack config", a.exportStackConfig)
	opr[idParam, struct{}](a, g, "/orgs/:id/config/export", ScopeConfigRead, service.VerbConfigExport, service.KindOrg, "id", "Export the org config", a.exportOrgConfig)
	opr[idParam, planDetailOut](a, g, "/config/plans/:id", ScopeConfigRead, service.VerbOrgRead, service.KindStackPlan, "id", "Get config plan", a.getPlan)
	opw[idParam, planDetailOut](a, g, http.MethodPost, "/config/plans/:id/approve", ScopeConfigApply, service.VerbStackPlanApprove, service.KindStackPlan, "id", "Approve config plan", a.approvePlan)
	opw[idParam, planDetailOut](a, g, http.MethodPost, "/config/plans/:id/reject", ScopeConfigApply, service.VerbStackPlanApprove, service.KindStackPlan, "id", "Reject config plan", a.rejectPlan)
	// deployments + logs
	opr[idParam, []deploymentOut](a, g, "/apps/:id/deployments", ScopeAppsRead, service.VerbTileRead, service.KindTile, "id", "List app deployments", a.listDeployments)
	opr[idParam, deploymentOut](a, g, "/deployments/:id", ScopeAppsRead, service.VerbDeploymentRead, service.KindDeployment, "id", "Get deployment", a.getDeployment)
	opr[logsIn, logsOut](a, g, "/apps/:id/logs", ScopeLogsRead, service.VerbTileRead, service.KindTile, "id", "Get app logs", a.getAppLogs)
	opr[idParam, logsOut](a, g, "/deployments/:id/logs", ScopeLogsRead, service.VerbDeploymentRead, service.KindDeployment, "id", "Get deployment logs", a.getDeploymentLogs)
	// databases
	op[struct{}, []dbOut](a, g, http.MethodGet, "/dbs", ScopeDBsRead, "List databases", a.listDBs)
	opr[idParam, dbOut](a, g, "/dbs/:id", ScopeDBsRead, service.VerbTileRead, service.KindTile, "id", "Get database", a.getDB)
	opw[dbIn, dbOut](a, g, http.MethodPost, "/stacks/:id/dbs", ScopeDBsWrite, service.VerbStackWrite, service.KindStack, "id", "Create database", a.createDB)
	opw[dbPatch, dbOut](a, g, http.MethodPatch, "/dbs/:id", ScopeDBsWrite, service.VerbTileWrite, service.KindTile, "id", "Update database", a.patchDB)
	opw[dbDelete, struct{}](a, g, http.MethodDelete, "/dbs/:id", ScopeDBsWrite, service.VerbTileWrite, service.KindTile, "id", "Delete database", a.deleteDB)
	// addressing: turn an infra path into the instance or slice it names, so
	// the CLI can speak paths while every other route stays keyed by id
	opw[resolveIn, resolveOut](a, g, http.MethodPost, "/resolve", ScopeDBsRead, service.VerbOrgRead, service.KindDeferred, "", "Resolve an infra path", a.resolvePath)
	// slices of a shared instance, independent of any consumer
	opr[idParam, []sliceOut](a, g, "/dbs/:id/provisions", ScopeDBsRead, service.VerbTileRead, service.KindTile, "id", "List instance slices", a.listInstanceProvisions)
	opw[sliceIn, sliceOut](a, g, http.MethodPost, "/dbs/:id/provisions", ScopeDBsWrite, service.VerbTileWrite, service.KindTile, "id", "Create a slice", a.createInstanceProvision)
	opw[idParam, struct{}](a, g, http.MethodDelete, "/provisions/:id", ScopeDBsWrite, service.VerbTileWrite, service.KindProvision, "id", "Delete a slice", a.deleteProvision)
	opw[publicIn, sliceOut](a, g, http.MethodPost, "/provisions/:id/public", ScopeDBsWrite, service.VerbTileWrite, service.KindProvision, "id", "Set a slice public or private", a.setProvisionPublic)
	opw[detachIn, struct{}](a, g, http.MethodPost, "/apps/:id/provisions/:pid/detach", ScopeDBsWrite, service.VerbTileWrite, service.KindTile, "id", "Detach a slice from an app", a.detachProvision)
	opw[forkIn, sliceOut](a, g, http.MethodPost, "/provisions/:id/fork", ScopeDBsFork, service.VerbTileWrite, service.KindProvision, "id", "Fork a slice", a.forkProvision)
	// provisioning: an app consumes a logical db from a shared instance
	opr[idParam, []provisionOut](a, g, "/apps/:id/provisions", ScopeDBsRead, service.VerbTileRead, service.KindTile, "id", "List app provisions", a.listAppProvisions)
	opw[provisionIn, provisionOut](a, g, http.MethodPost, "/apps/:id/provision", ScopeDBsWrite, service.VerbTileWrite, service.KindTile, "id", "Provision database", a.provisionApp)
	opw[attachIn, provisionOut](a, g, http.MethodPost, "/apps/:id/provisions/attach", ScopeDBsWrite, service.VerbTileWrite, service.KindTile, "id", "Attach database", a.attachProvision)
	// domains
	opr[idParam, []domainOut](a, g, "/apps/:id/domains", ScopeAppsRead, service.VerbTileRead, service.KindTile, "id", "List app domains", a.listDomains)
	opw[domainIn, domainOut](a, g, http.MethodPost, "/apps/:id/domains", ScopeDomainsWrite, service.VerbDomainWrite, service.KindTile, "id", "Add domain", a.createDomain)
	opw[idParam, domainOut](a, g, http.MethodPost, "/apps/:id/domains/auto", ScopeDomainsWrite, service.VerbDomainWrite, service.KindTile, "id", "Generate the app's auto domain", a.createAutoDomain)
	opw[domainPatch, domainOut](a, g, http.MethodPatch, "/domains/:id", ScopeDomainsWrite, service.VerbDomainWrite, service.KindDomain, "id", "Update domain TLS", a.patchDomain)
	opw[idParam, struct{}](a, g, http.MethodDelete, "/domains/:id", ScopeDomainsWrite, service.VerbDomainWrite, service.KindDomain, "id", "Remove domain", a.deleteDomain)
	// domain resources: hosts that auto-generated tile hostnames nest under
	op[struct{}, []domainResourceOut](a, g, http.MethodGet, "/domain-resources", ScopeStacksRead, "List domain resources", a.listDomainResources)
	opw[domainResourceIn, domainResourceOut](a, g, http.MethodPost, "/domain-resources", ScopeDomainsWrite, service.VerbDomainWrite, service.KindDeferred, "", "Create domain resource", a.createDomainResource)
	opw[domainResourcePatch, domainResourceOut](a, g, http.MethodPatch, "/domain-resources/:id", ScopeDomainsWrite, service.VerbDomainWrite, service.KindDomainResource, "id", "Set a domain resource's ACME account", a.patchDomainResource)
	opw[idParam, struct{}](a, g, http.MethodDelete, "/domain-resources/:id", ScopeDomainsWrite, service.VerbDomainWrite, service.KindDomainResource, "id", "Delete domain resource", a.deleteDomainResource)
	// volumes
	opr[idParam, []volumeOut](a, g, "/apps/:id/volumes", ScopeAppsRead, service.VerbTileRead, service.KindTile, "id", "List app volumes", a.listVolumes)
	opw[volumeIn, volumeOut](a, g, http.MethodPost, "/apps/:id/volumes", ScopeAppsWrite, service.VerbTileWrite, service.KindTile, "id", "Add volume", a.createVolume)
	opw[idParam, struct{}](a, g, http.MethodDelete, "/volumes/:id", ScopeAppsWrite, service.VerbTileWrite, service.KindTile, "id", "Remove volume", a.deleteVolume)
	// backups: destinations are org-scoped (or server-wide), schedules hang off
	// a tile. "tiles" and not "dbs" because volume tiles get backed up too.
	op[struct{}, []destinationOut](a, g, http.MethodGet, "/backup-destinations", ScopeBackupsRead, "List backup destinations", a.listDestinations)
	opw[destinationIn, destinationOut](a, g, http.MethodPost, "/backup-destinations", ScopeBackupsWrite, service.VerbDestinationWrite, service.KindDeferred, "", "Create backup destination", a.createDestination)
	opw[destinationPatch, destinationOut](a, g, http.MethodPatch, "/backup-destinations/:id", ScopeBackupsWrite, service.VerbServerDefaults, service.KindNone, "", "Share or unshare a server-wide destination", a.patchDestination)
	opw[idParam, struct{}](a, g, http.MethodDelete, "/backup-destinations/:id", ScopeBackupsWrite, service.VerbDestinationWrite, service.KindDestination, "id", "Delete backup destination", a.deleteDestination)
	opr[idParam, []backupOut](a, g, "/tiles/:id/backups", ScopeBackupsRead, service.VerbTileRead, service.KindTile, "id", "List tile backups", a.listBackups)
	opw[backupIn, backupOut](a, g, http.MethodPost, "/tiles/:id/backups", ScopeBackupsWrite, service.VerbBackupWrite, service.KindTile, "id", "Create backup", a.createBackup)
	opw[backupPatch, backupOut](a, g, http.MethodPatch, "/backups/:id", ScopeBackupsWrite, service.VerbBackupWrite, service.KindBackup, "id", "Update backup", a.patchBackup)
	opw[idParam, struct{}](a, g, http.MethodDelete, "/backups/:id", ScopeBackupsWrite, service.VerbBackupWrite, service.KindBackup, "id", "Delete backup", a.deleteBackup)
	opr[idParam, []backupRunOut](a, g, "/backups/:id/runs", ScopeBackupsRead, service.VerbOrgRead, service.KindBackup, "id", "List backup runs", a.listBackupRuns)
	opw[idParam, backupRunOut](a, g, http.MethodPost, "/backups/:id/run", ScopeBackupsWrite, service.VerbBackupWrite, service.KindBackup, "id", "Run backup now", a.runBackup)
	opw[restoreIn, restoreOut](a, g, http.MethodPost, "/backups/:id/restore", ScopeBackupsRestore, service.VerbBackupWrite, service.KindBackup, "id", "Restore from a backup", a.restoreBackup)
	opr[idParam, restoreOut](a, g, "/backups/:id/restore", ScopeBackupsRead, service.VerbOrgRead, service.KindBackup, "id", "Latest restore of a backup", a.getRestore)
	// port-forward: a websocket tunnel to a container port. "tiles" and not
	// "apps"/"dbs" because it works on either, which those paths don't say.
	// The spec entry is nominal, the response is a 101 upgrade, not JSON.
	opr[forwardIn, struct{}](a, g, "/tiles/:id/forward", ScopeTilesForward, service.VerbTileWrite, service.KindTile, "id", "Port-forward a tile", a.forwardTile)
}

// SpecJSON returns the generated OpenAPI document as JSON bytes (used by the
// live endpoint and the `make openapi` generator).
func (a *API) SpecJSON() ([]byte, error) {
	return a.spec.Spec.MarshalJSON()
}

// SpecHandler serves the generated OpenAPI document.
func (a *API) SpecHandler(c echo.Context) error {
	b, err := a.SpecJSON()
	if err != nil {
		return err
	}
	return c.Blob(http.StatusOK, "application/json", b)
}
