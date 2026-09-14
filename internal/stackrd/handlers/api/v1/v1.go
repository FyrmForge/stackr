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

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/forward"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type API struct {
	store   repo.Store
	engine  *deploy.Engine
	rt      *runtime.Runtime
	clus    *cluster.Cluster
	work    *workqueue.Queue
	px      *proxy.Proxy
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
	spec   *openapi3.Reflector
}

// securityName is the OpenAPI security scheme id for the x-api-key header.
const securityName = "apiKey"

func New(store repo.Store, engine *deploy.Engine, rt *runtime.Runtime, clus *cluster.Cluster, px *proxy.Proxy, js *jobs.Service, bk *backup.Service, fwd *forward.Registry, n *notify.Notifier, ap stackconf.Applier) *API {
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

// op registers a route (guarded by its required scope) and mirrors it into the
// OpenAPI spec, the route, its scope, and its spec entry all come from one
// call, so the published spec can't drift from what's enforced.
//
// desc is an optional longer note for the spec. It is variadic so the summary
// stays the short name every operation id is derived from: a summary long
// enough to explain a side effect would rename the generated client method.
func op[Req, Resp any](a *API, g *echo.Group, method, path, scope, summary string, h echo.HandlerFunc, desc ...string) {
	g.Add(method, path, a.requireScope(scope, h))
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
	var req Req
	var resp Resp
	oc.AddReqStructure(req)
	oc.AddRespStructure(resp, withStatus(successStatus(method, path)))
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
	op[struct{}, settingsOut](a, g, http.MethodGet, "/settings", ScopeStacksRead, "Get server defaults", a.settingsFor("server"))
	op[map[string]*string, settingsOut](a, g, http.MethodPatch, "/settings", ScopeStacksWrite, "Set server defaults", a.patchSettingsFor("server"))
	op[idParam, settingsOut](a, g, http.MethodGet, "/orgs/:id/settings", ScopeStacksRead, "Get organization defaults", a.settingsFor("org"))
	op[idParam, settingsOut](a, g, http.MethodPatch, "/orgs/:id/settings", ScopeStacksWrite, "Set organization defaults", a.patchSettingsFor("org"))
	op[idParam, settingsOut](a, g, http.MethodGet, "/stacks/:id/settings", ScopeStacksRead, "Get stack defaults", a.settingsFor("stack"))
	op[idParam, settingsOut](a, g, http.MethodPatch, "/stacks/:id/settings", ScopeStacksWrite, "Set stack defaults", a.patchSettingsFor("stack"))
	op[idParam, settingsOut](a, g, http.MethodGet, "/envs/:id/settings", ScopeStacksRead, "Get environment defaults", a.settingsFor("env"))
	op[idParam, settingsOut](a, g, http.MethodPatch, "/envs/:id/settings", ScopeEnvsWrite, "Set environment defaults", a.patchSettingsFor("env"))
	// orgs: the slug every path form starts with, so this is read-only and
	// carries the caller's own role rather than the org's members.
	op[struct{}, []orgOut](a, g, http.MethodGet, "/orgs", ScopeStacksRead, "List organizations", a.listOrgs)
	// registries: the managed one's TLS domain and the external ones a tile can
	// pull from. Server-wide, so admin-only.
	op[struct{}, []registryOut](a, g, http.MethodGet, "/registries", ScopeStacksRead, "List registries", a.adminOnly(a.listRegistries))
	op[registryIn, registryOut](a, g, http.MethodPost, "/registries", ScopeStacksWrite, "Add an external registry", a.adminOnly(a.createRegistry))
	op[registryPatch, registryOut](a, g, http.MethodPatch, "/registries/:id", ScopeStacksWrite, "Update a registry", a.adminOnly(a.patchRegistry))
	op[idParam, struct{}](a, g, http.MethodDelete, "/registries/:id", ScopeStacksWrite, "Remove an external registry", a.adminOnly(a.deleteRegistry))
	// members, roles and invites: onboarding a person was panel-only, so it
	// was the one thing nobody could script.
	op[idParam, []memberOut](a, g, http.MethodGet, "/orgs/:id/members", ScopeStacksRead, "List organization members", a.listMembers)
	op[inviteIn, inviteOut](a, g, http.MethodPost, "/orgs/:id/members", ScopeStacksWrite, "Invite a member", a.addMember)
	op[roleIn, memberOut](a, g, http.MethodPatch, "/orgs/:id/members/:user", ScopeStacksWrite, "Change a member's role", a.setMemberRole)
	op[roleIn, struct{}](a, g, http.MethodDelete, "/orgs/:id/members/:user", ScopeStacksWrite, "Remove a member", a.removeMember)
	op[idParam, []inviteOut](a, g, http.MethodGet, "/orgs/:id/invites", ScopeStacksRead, "List open invites", a.listInvites)
	op[inviteIn, inviteOut](a, g, http.MethodPost, "/orgs/:id/invites", ScopeStacksWrite, "Create an invite", a.createInvite)
	op[inviteParam, struct{}](a, g, http.MethodDelete, "/orgs/:id/invites/:invite", ScopeStacksWrite, "Revoke an invite", a.deleteInvite)
	// org registry: push credentials and the images they produced. Reads are
	// filtered to the caller's namespace here; the registry enforces the same
	// boundary on the wire through scoped tokens.
	op[idParam, []registryCredentialOut](a, g, http.MethodGet, "/orgs/:id/registry/credentials", ScopeStacksRead, "List registry credentials", a.listRegistryCredentials)
	op[registryCredentialIn, registryCredentialOut](a, g, http.MethodPost, "/orgs/:id/registry/credentials", ScopeStacksWrite, "Create a registry credential", a.createRegistryCredential)
	op[registryCredParam, struct{}](a, g, http.MethodDelete, "/orgs/:id/registry/credentials/:cred", ScopeStacksWrite, "Delete a registry credential", a.deleteRegistryCredential)
	op[idParam, []registryImageOut](a, g, http.MethodGet, "/orgs/:id/registry/images", ScopeStacksRead, "List registry images", a.listRegistryImages)
	op[registryImageParam, []registryTagOut](a, g, http.MethodGet, "/orgs/:id/registry/images/:name/tags", ScopeStacksRead, "List an image's tags", a.listRegistryTags)
	op[registryTagParam, struct{}](a, g, http.MethodDelete, "/orgs/:id/registry/images/:name/tags/:tag", ScopeStacksWrite, "Delete an image tag", a.deleteRegistryTag)
	// stacks
	op[struct{}, []stackOut](a, g, http.MethodGet, "/stacks", ScopeStacksRead, "List stacks", a.listStacks)
	op[stackIn, stackOut](a, g, http.MethodPost, "/stacks", ScopeStacksWrite, "Create stack", a.createStack)
	op[idParam, struct{}](a, g, http.MethodDelete, "/stacks/:id", ScopeStacksWrite, "Delete stack", a.deleteStack)
	op[idParam, []envOut](a, g, http.MethodGet, "/stacks/:id/envs", ScopeStacksRead, "List environments", a.listEnvs)
	op[envIn, envOut](a, g, http.MethodPost, "/stacks/:id/envs", ScopeEnvsWrite, "Create environment", a.createEnv)
	// promotion: what is promotable, and moving one commit up a rung
	// environment lifecycle
	op[forceIn, struct{}](a, g, http.MethodDelete, "/envs/:id", ScopeEnvsWrite, "Delete environment", a.deleteEnv)
	op[forceIn, struct{}](a, g, http.MethodPost, "/envs/:id/reset", ScopeEnvsWrite, "Reset a config-managed environment", a.resetEnv)
	op[copyEnvIn, envOut](a, g, http.MethodPost, "/envs/:id/copy", ScopeEnvsWrite, "Copy an environment", a.copyEnv)
	op[envPatch, envOut](a, g, http.MethodPatch, "/envs/:id", ScopeEnvsWrite, "Update environment settings", a.patchEnv)
	op[idParam, []releaseOut](a, g, http.MethodGet, "/stacks/:id/releases", ScopeAppsRead, "List releases", a.listReleases)
	op[promoteIn, promoteOut](a, g, http.MethodPost, "/stacks/:id/envs/:slug/promote", ScopeAppsDeploy, "Promote a commit to an environment", a.promote)
	// apps
	op[appsQuery, []appOut](a, g, http.MethodGet, "/apps", ScopeAppsRead, "List apps", a.listApps)
	op[appIn, appOut](a, g, http.MethodPost, "/stacks/:id/apps", ScopeAppsWrite, "Create app", a.createApp)
	op[idParam, appOut](a, g, http.MethodGet, "/apps/:id", ScopeAppsRead, "Get app", a.getApp)
	op[appPatch, appOut](a, g, http.MethodPatch, "/apps/:id", ScopeAppsWrite, "Update app", a.patchApp)
	op[idParam, struct{}](a, g, http.MethodDelete, "/apps/:id", ScopeAppsWrite, "Delete app", a.deleteApp)
	op[idParam, deployAccepted](a, g, http.MethodPost, "/apps/:id/deploy", ScopeAppsDeploy, "Trigger deployment", a.deployApp)
	op[idParam, runAccepted](a, g, http.MethodPost, "/apps/:id/run", ScopeAppsDeploy, "Run now (cron/function one-shot)", a.runApp)
	op[runStopIn, runStopped](a, g, http.MethodPost, "/apps/:id/runs/:run/stop", ScopeAppsDeploy, "Stop a run in flight", a.stopRun)
	// tile lifecycle: the panel's buttons, so a script can do what a person can
	op[idParam, appOut](a, g, http.MethodPost, "/apps/:id/stop", ScopeAppsDeploy, "Stop app", a.stopApp)
	op[idParam, appOut](a, g, http.MethodPost, "/apps/:id/restart", ScopeAppsDeploy, "Restart app", a.restartApp)
	op[idParam, appOut](a, g, http.MethodPost, "/apps/:id/cron/toggle", ScopeAppsDeploy, "Pause or resume a cron schedule", a.toggleCron)
	op[rollbackIn, deployAccepted](a, g, http.MethodPost, "/apps/:id/rollback", ScopeAppsDeploy, "Roll back to an image tag", a.rollbackApp)
	op[idParam, deploymentOut](a, g, http.MethodPost, "/deployments/:id/cancel", ScopeAppsDeploy, "Cancel a deployment", a.cancelDeployment)
	op[idParam, []metricOut](a, g, http.MethodGet, "/apps/:id/metrics", ScopeAppsRead, "Get app metrics", a.appMetrics)
	// stack settings the panel owned alone
	op[stackPatch, stackOut](a, g, http.MethodPatch, "/stacks/:id", ScopeStacksWrite, "Rename stack", a.patchStack,
		"The slug moves with the name, so every URL and container name under the stack moves too. Swarm cannot rename a service, so running tiles are stopped under the old name and redeployed under the new one. Volumes, databases and history are keyed by id and survive. A config-managed stack is refused: its file owns the name.")
	op[idParam, prEnvOut](a, g, http.MethodGet, "/stacks/:id/pr-envs", ScopeStacksRead, "Get PR environment settings", a.getPREnv)
	op[prEnvIn, prEnvOut](a, g, http.MethodPut, "/stacks/:id/pr-envs", ScopeStacksWrite, "Set PR environment settings", a.putPREnv)
	// org config-as-code (§6): plans only, the binding is panel-owned
	op[idParam, planOut](a, g, http.MethodPost, "/orgs/:id/config/plan", ScopeConfigApply, "Plan org config", a.planOrg)
	op[idParam, []planOut](a, g, http.MethodGet, "/orgs/:id/config/plans", ScopeConfigRead, "List org config plans", a.listOrgPlans)
	op[orgPreviewIn, planDetailOut](a, g, http.MethodPost, "/orgs/:id/config/plan-preview", ScopeConfigRead, "Preview org config plan", a.previewOrgPlan)
	op[idParam, planOut](a, g, http.MethodPost, "/org-config/plans/:id/approve", ScopeConfigApply, "Approve + apply an org plan", a.approveOrgPlan)
	op[idParam, planOut](a, g, http.MethodPost, "/org-config/plans/:id/reject", ScopeConfigApply, "Reject an org plan", a.rejectOrgPlan)
	// storage tiles (server-wide shares/pools; attachments ride the tile PATCH)
	op[struct{}, []storageOut](a, g, http.MethodGet, "/storage", ScopeStorageRead, "List storage", a.adminOnly(a.listStorage))
	op[storageIn, storageOut](a, g, http.MethodPost, "/storage", ScopeStorageWrite, "Create storage (probes on create)", a.adminOnly(a.createStorage))
	op[idParam, storageOut](a, g, http.MethodPost, "/storage/:id/probe", ScopeStorageWrite, "Probe storage", a.adminOnly(a.probeStorage))
	op[idParam, struct{}](a, g, http.MethodDelete, "/storage/:id", ScopeStorageWrite, "Delete storage", a.adminOnly(a.deleteStorage))
	op[storagePathIn, storagePathOut](a, g, http.MethodPost, "/storage/:id/paths", ScopeStorageWrite, "Declare a sub-path", a.adminOnly(a.createStoragePath))
	op[idParam, struct{}](a, g, http.MethodDelete, "/storage/paths/:id", ScopeStorageWrite, "Remove a sub-path", a.adminOnly(a.deleteStoragePath))
	// proxy escape hatches (server-wide; domains:write is the closest scope)
	op[struct{}, proxyConfigOut](a, g, http.MethodGet, "/proxy/config", ScopeDomainsWrite, "Get proxy config + escape hatches", a.adminOnly(a.getProxyConfig))
	op[proxyOverrideIn, proxyConfigOut](a, g, http.MethodPut, "/proxy/static-override", ScopeDomainsWrite, "Set/clear the static traefik.yml override", a.adminOnly(a.putProxyOverride))
	op[proxyEntryIn, proxyConfigOut](a, g, http.MethodPut, "/proxy/entries/:name", ScopeDomainsWrite, "Write a custom dynamic entry", a.adminOnly(a.putProxyEntry))
	op[proxyEntryParam, struct{}](a, g, http.MethodDelete, "/proxy/entries/:name", ScopeDomainsWrite, "Delete a custom dynamic entry", a.adminOnly(a.deleteProxyEntry))
	// variables
	op[idParam, varsOut](a, g, http.MethodGet, "/apps/:id/variables", ScopeVarsRead, "Get app variables", a.getVars)
	op[varsIn, varsOut](a, g, http.MethodPut, "/apps/:id/variables", ScopeVarsWrite, "Replace app variables", a.putVars)
	// The stored expressions verbatim, what to edit, as opposed to what a
	// container will see. Same handler as GET /variables: the name is what
	// pairs with /resolved, and a second copy of the body would be a second
	// place for the audit rules to drift.
	op[idParam, varsOut](a, g, http.MethodGet, "/apps/:id/variables/unresolved", ScopeVarsRead, "Get stored variable expressions", a.getVars)
	op[idParam, varsOut](a, g, http.MethodGet, "/apps/:id/variables/resolved", ScopeVarsRead, "Get resolved variables", a.resolvedVars)
	op[idParam, []refSourceOut](a, g, http.MethodGet, "/apps/:id/reference-catalogue", ScopeVarsRead, "List referenceable sources", a.referenceCatalogue)
	op[idParam, varsOut](a, g, http.MethodGet, "/stacks/:id/variables", ScopeVarsRead, "Get stack variables", a.getStackVars)
	op[varsIn, varsOut](a, g, http.MethodPut, "/stacks/:id/variables", ScopeVarsWrite, "Replace stack variables", a.putStackVars)
	op[idParam, varsOut](a, g, http.MethodGet, "/envs/:id/variables", ScopeVarsRead, "Get environment variables", a.getEnvVars)
	op[varsIn, varsOut](a, g, http.MethodPut, "/envs/:id/variables", ScopeVarsWrite, "Replace environment variables", a.putEnvVars)
	op[idParam, varsOut](a, g, http.MethodGet, "/orgs/:id/variables", ScopeVarsRead, "Get org variables", a.getOrgVars)
	op[varsIn, varsOut](a, g, http.MethodPut, "/orgs/:id/variables", ScopeVarsWrite, "Replace org variables", a.putOrgVars)
	op[idParam, []resourceOut](a, g, http.MethodGet, "/apps/:id/resources", ScopeDBsRead, "List app managed resources", a.listAppResources)
	// config-as-code: plans are proposed by a push (or by planning now) and
	// applied by approval, which is what a gated environment's manual policy
	// waits for.
	op[plansQuery, []planOut](a, g, http.MethodGet, "/stacks/:id/config/plans", ScopeConfigRead, "List config plans", a.listPlans)
	op[idParam, []planDetailOut](a, g, http.MethodPost, "/stacks/:id/config/plan", ScopeConfigApply, "Create config plan", a.planStack)
	op[previewIn, planDetailOut](a, g, http.MethodPost, "/stacks/:id/config/plan-preview", ScopeConfigRead, "Preview config plan", a.previewPlan)
	// export: live state as the file that would produce it, so a panel-first
	// stack can move to config-as-code without hand-writing one.
	op[idParam, struct{}](a, g, http.MethodGet, "/stacks/:id/config/export", ScopeConfigRead, "Export the stack config", a.exportStackConfig)
	op[idParam, struct{}](a, g, http.MethodGet, "/orgs/:id/config/export", ScopeConfigRead, "Export the org config", a.exportOrgConfig)
	op[idParam, planDetailOut](a, g, http.MethodGet, "/config/plans/:id", ScopeConfigRead, "Get config plan", a.getPlan)
	op[idParam, planDetailOut](a, g, http.MethodPost, "/config/plans/:id/approve", ScopeConfigApply, "Approve config plan", a.approvePlan)
	op[idParam, planDetailOut](a, g, http.MethodPost, "/config/plans/:id/reject", ScopeConfigApply, "Reject config plan", a.rejectPlan)
	// deployments + logs
	op[idParam, []deploymentOut](a, g, http.MethodGet, "/apps/:id/deployments", ScopeAppsRead, "List app deployments", a.listDeployments)
	op[idParam, deploymentOut](a, g, http.MethodGet, "/deployments/:id", ScopeAppsRead, "Get deployment", a.getDeployment)
	op[logsIn, logsOut](a, g, http.MethodGet, "/apps/:id/logs", ScopeLogsRead, "Get app logs", a.getAppLogs)
	op[idParam, logsOut](a, g, http.MethodGet, "/deployments/:id/logs", ScopeLogsRead, "Get deployment logs", a.getDeploymentLogs)
	// databases
	op[struct{}, []dbOut](a, g, http.MethodGet, "/dbs", ScopeDBsRead, "List databases", a.listDBs)
	op[idParam, dbOut](a, g, http.MethodGet, "/dbs/:id", ScopeDBsRead, "Get database", a.getDB)
	op[dbIn, dbOut](a, g, http.MethodPost, "/stacks/:id/dbs", ScopeDBsWrite, "Create database", a.createDB)
	op[dbPatch, dbOut](a, g, http.MethodPatch, "/dbs/:id", ScopeDBsWrite, "Update database", a.patchDB)
	op[dbDelete, struct{}](a, g, http.MethodDelete, "/dbs/:id", ScopeDBsWrite, "Delete database", a.deleteDB)
	// addressing: turn an infra path into the instance or slice it names, so
	// the CLI can speak paths while every other route stays keyed by id
	op[resolveIn, resolveOut](a, g, http.MethodPost, "/resolve", ScopeDBsRead, "Resolve an infra path", a.resolvePath)
	// slices of a shared instance, independent of any consumer
	op[idParam, []sliceOut](a, g, http.MethodGet, "/dbs/:id/provisions", ScopeDBsRead, "List instance slices", a.listInstanceProvisions)
	op[sliceIn, sliceOut](a, g, http.MethodPost, "/dbs/:id/provisions", ScopeDBsWrite, "Create a slice", a.createInstanceProvision)
	op[idParam, struct{}](a, g, http.MethodDelete, "/provisions/:id", ScopeDBsWrite, "Delete a slice", a.deleteProvision)
	op[publicIn, sliceOut](a, g, http.MethodPost, "/provisions/:id/public", ScopeDBsWrite, "Set a slice public or private", a.setProvisionPublic)
	op[detachIn, struct{}](a, g, http.MethodPost, "/apps/:id/provisions/:pid/detach", ScopeDBsWrite, "Detach a slice from an app", a.detachProvision)
	op[forkIn, sliceOut](a, g, http.MethodPost, "/provisions/:id/fork", ScopeDBsFork, "Fork a slice", a.forkProvision)
	// provisioning: an app consumes a logical db from a shared instance
	op[idParam, []provisionOut](a, g, http.MethodGet, "/apps/:id/provisions", ScopeDBsRead, "List app provisions", a.listAppProvisions)
	op[provisionIn, provisionOut](a, g, http.MethodPost, "/apps/:id/provision", ScopeDBsWrite, "Provision database", a.provisionApp)
	op[attachIn, provisionOut](a, g, http.MethodPost, "/apps/:id/provisions/attach", ScopeDBsWrite, "Attach database", a.attachProvision)
	// domains
	op[idParam, []domainOut](a, g, http.MethodGet, "/apps/:id/domains", ScopeAppsRead, "List app domains", a.listDomains)
	op[domainIn, domainOut](a, g, http.MethodPost, "/apps/:id/domains", ScopeDomainsWrite, "Add domain", a.createDomain)
	op[idParam, domainOut](a, g, http.MethodPost, "/apps/:id/domains/auto", ScopeDomainsWrite, "Generate the app's auto domain", a.createAutoDomain)
	op[domainPatch, domainOut](a, g, http.MethodPatch, "/domains/:id", ScopeDomainsWrite, "Update domain TLS", a.patchDomain)
	op[idParam, struct{}](a, g, http.MethodDelete, "/domains/:id", ScopeDomainsWrite, "Remove domain", a.deleteDomain)
	// domain resources: hosts that auto-generated tile hostnames nest under
	op[struct{}, []domainResourceOut](a, g, http.MethodGet, "/domain-resources", ScopeStacksRead, "List domain resources", a.listDomainResources)
	op[domainResourceIn, domainResourceOut](a, g, http.MethodPost, "/domain-resources", ScopeDomainsWrite, "Create domain resource", a.createDomainResource)
	op[domainResourcePatch, domainResourceOut](a, g, http.MethodPatch, "/domain-resources/:id", ScopeDomainsWrite, "Set a domain resource's ACME account", a.patchDomainResource)
	op[idParam, struct{}](a, g, http.MethodDelete, "/domain-resources/:id", ScopeDomainsWrite, "Delete domain resource", a.deleteDomainResource)
	// volumes
	op[idParam, []volumeOut](a, g, http.MethodGet, "/apps/:id/volumes", ScopeAppsRead, "List app volumes", a.listVolumes)
	op[volumeIn, volumeOut](a, g, http.MethodPost, "/apps/:id/volumes", ScopeAppsWrite, "Add volume", a.createVolume)
	op[idParam, struct{}](a, g, http.MethodDelete, "/volumes/:id", ScopeAppsWrite, "Remove volume", a.deleteVolume)
	// backups: destinations are org-scoped (or server-wide), schedules hang off
	// a tile. "tiles" and not "dbs" because volume tiles get backed up too.
	op[struct{}, []destinationOut](a, g, http.MethodGet, "/backup-destinations", ScopeBackupsRead, "List backup destinations", a.listDestinations)
	op[destinationIn, destinationOut](a, g, http.MethodPost, "/backup-destinations", ScopeBackupsWrite, "Create backup destination", a.createDestination)
	op[destinationPatch, destinationOut](a, g, http.MethodPatch, "/backup-destinations/:id", ScopeBackupsWrite, "Share or unshare a server-wide destination", a.patchDestination)
	op[idParam, struct{}](a, g, http.MethodDelete, "/backup-destinations/:id", ScopeBackupsWrite, "Delete backup destination", a.deleteDestination)
	op[idParam, []backupOut](a, g, http.MethodGet, "/tiles/:id/backups", ScopeBackupsRead, "List tile backups", a.listBackups)
	op[backupIn, backupOut](a, g, http.MethodPost, "/tiles/:id/backups", ScopeBackupsWrite, "Create backup", a.createBackup)
	op[backupPatch, backupOut](a, g, http.MethodPatch, "/backups/:id", ScopeBackupsWrite, "Update backup", a.patchBackup)
	op[idParam, struct{}](a, g, http.MethodDelete, "/backups/:id", ScopeBackupsWrite, "Delete backup", a.deleteBackup)
	op[idParam, []backupRunOut](a, g, http.MethodGet, "/backups/:id/runs", ScopeBackupsRead, "List backup runs", a.listBackupRuns)
	op[idParam, backupRunOut](a, g, http.MethodPost, "/backups/:id/run", ScopeBackupsWrite, "Run backup now", a.runBackup)
	op[restoreIn, struct{}](a, g, http.MethodPost, "/backups/:id/restore", ScopeBackupsRestore, "Restore from a backup", a.restoreBackup)
	// port-forward: a websocket tunnel to a container port. "tiles" and not
	// "apps"/"dbs" because it works on either, which those paths don't say.
	// The spec entry is nominal, the response is a 101 upgrade, not JSON.
	op[forwardIn, struct{}](a, g, http.MethodGet, "/tiles/:id/forward", ScopeTilesForward, "Port-forward a tile", a.forwardTile)
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
