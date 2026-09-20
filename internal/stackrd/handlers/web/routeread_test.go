package web_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
)

// The read side of point 18, and its captured levels.
//
// The writes went first, for a reason recorded in 06-points-18-20.md: for a
// mutating handler the gate helper is separable from the work, so an AST walk
// could resolve what each route enforced and freeze it before the helpers were
// deleted. For a read-only handler the gate helper IS the body — resolve the
// org, refuse, render — so the same walk answers "this page reaches
// RequireOrgWrite" about a page that is read-only to a member. The union it
// produces is an upper bound, not a level.
//
// So this table was NOT taken from the walk. The walk narrowed 192 GET routes
// to 22 that reach an owner-level helper, and those 22 were read by hand; the
// three things that came out of it are recorded in 05-assumptions.md:
//
//   - RequireOrgWrite is membership-only on a GET. It refuses on `mutating(c)`
//     alone, so every settings page reaching it is a read-level page.
//   - ownedSettingsOrg is the real owner gate, and answers 404, not 403.
//   - four "reveal this variable's value" GETs check CanWriteOrg explicitly,
//     which makes them write-level reads.
//
// Columns are the mutating table's: route, level, and WHICH ORG the level is
// checked against. Levels are the ladder in service/access.go.
const capturedReadLevels = `
GET /:org/:stack	read	resource	# web/handler/project.StackGraph
GET /:org/:stack/:env	read	resource	# web/handler/project.Graph
GET /:org/:stack/:env/:tile	read	resource	# web.tilePage.func1
GET /:org/:stack/:env/logs	read	resource	# web/handler/project.EnvLogs
GET /:org/:stack/plans/:planID	read	resource	# web/handler/project.PlanView
GET /:org/:stack/releases	read	resource	# web/handler/project.Releases
GET /:org/:stack/settings	read	resource	# web/handler/project.Settings
GET /:org/:stack/settings/config	read	resource	# web/handler/project.SettingsConfig
GET /:org/:stack/settings/config/export	read	resource	# web/handler/project.ExportConfig
GET /:org/:stack/settings/domains	read	resource	# web/handler/project.SettingsDomains
GET /:org/:stack/settings/environments	read	resource	# web/handler/project.SettingsEnvironments
GET /:org/:stack/settings/environments/:env	read	resource	# web/handler/project.SettingsEnvironment
GET /:org/:stack/settings/environments/:env/variables/panel	read	resource	# web/handler/project.EnvVarsPanel
GET /:org/:stack/settings/environments/:env/variables/value	write	resource	# web/handler/project.EnvVarValue
GET /:org/:stack/settings/general	read	resource	# web/handler/project.SettingsGeneral
GET /:org/:stack/settings/pr	read	resource	# web/handler/project.SettingsPREnv
GET /:org/:stack/settings/variables	read	resource	# web/handler/project.SettingsVariables
GET /:org/:stack/settings/variables/panel	read	resource	# web/handler/project.StackVarsPanel
GET /:org/:stack/settings/variables/value	write	resource	# web/handler/project.StackVarValue
GET /admin	admin	n/a	# web/handler/settings.Index
GET /admin/audit	admin	n/a	# web/handler/settings.Audit
GET /admin/backups	admin	n/a	# web/handler/settings.Backups
GET /admin/maintenance	admin	n/a	# web/handler/settings.Maintenance
GET /admin/proxy	admin	n/a	# web/handler/settings.ProxyPage
GET /admin/registries	admin	n/a	# web/handler/settings.Registries
GET /admin/tls	admin	n/a	# web/handler/settings.TLS
GET /admin/update	admin	n/a	# web/handler/settings.Update
GET /admin/update/badge	admin	n/a	# web/handler/settings.UpdateBadge
GET /admin/update/check	admin	n/a	# web/handler/settings.UpdateCheck
GET /admin/users	admin	n/a	# web/handler/settings.Users
GET /api/v1/apps/:id	read	resource	# api/v1.getApp
GET /api/v1/apps/:id/deployments	read	resource	# api/v1.listDeployments
GET /api/v1/apps/:id/domains	read	resource	# api/v1.listDomains
GET /api/v1/apps/:id/logs	read	resource	# api/v1.getAppLogs
GET /api/v1/apps/:id/metrics	read	resource	# api/v1.appMetrics
GET /api/v1/apps/:id/provisions	read	resource	# api/v1.listAppProvisions
GET /api/v1/apps/:id/reference-catalogue	read	resource	# api/v1.referenceCatalogue
GET /api/v1/apps/:id/resources	read	resource	# api/v1.listAppResources
GET /api/v1/apps/:id/variables	read	resource	# api/v1.getVars
GET /api/v1/apps/:id/variables/resolved	read	resource	# api/v1.resolvedVars
GET /api/v1/apps/:id/variables/unresolved	read	resource	# api/v1.getVars
GET /api/v1/apps/:id/volumes	read	resource	# api/v1.listVolumes
GET /api/v1/backups/:id/restore	read	resource	# api/v1.getRestore
GET /api/v1/backups/:id/runs	read	resource	# api/v1.listBackupRuns
GET /api/v1/config/plans/:id	read	resource	# api/v1.getPlan
GET /api/v1/dbs/:id	read	resource	# api/v1.getDB
GET /api/v1/dbs/:id/provisions	read	resource	# api/v1.listInstanceProvisions
GET /api/v1/deployments/:id	read	resource	# api/v1.getDeployment
GET /api/v1/deployments/:id/logs	read	resource	# api/v1.getDeploymentLogs
GET /api/v1/envs/:id/settings	read	resource	# api/v1.settingsFor
GET /api/v1/envs/:id/variables	read	resource	# api/v1.getEnvVars
GET /api/v1/org-config/plans/:id	owner	resource	# api/v1.getOrgPlan
GET /api/v1/orgs/:id/config/export	read	resource	# api/v1.exportOrgConfig
GET /api/v1/orgs/:id/config/plans	owner	resource	# api/v1.listOrgPlans
GET /api/v1/orgs/:id/invites	owner	resource	# api/v1.listInvites
GET /api/v1/orgs/:id/members	read	resource	# api/v1.listMembers
GET /api/v1/orgs/:id/registry/credentials	read	resource	# api/v1.listRegistryCredentials
GET /api/v1/orgs/:id/registry/images	read	resource	# api/v1.listRegistryImages
GET /api/v1/orgs/:id/registry/images/:name/tags	read	resource	# api/v1.listRegistryTags
GET /api/v1/orgs/:id/settings	read	resource	# api/v1.settingsFor
GET /api/v1/orgs/:id/variables	read	resource	# api/v1.getOrgVars
GET /api/v1/proxy/config	admin	n/a	# api/v1.adminOnly
GET /api/v1/registries	admin	n/a	# api/v1.adminOnly
GET /api/v1/settings	admin	n/a	# api/v1.settingsFor
GET /api/v1/stacks/:id/config/export	read	resource	# api/v1.exportStackConfig
GET /api/v1/stacks/:id/config/plans	read	resource	# api/v1.listPlans
GET /api/v1/stacks/:id/envs	read	resource	# api/v1.listEnvs
GET /api/v1/stacks/:id/pr-envs	read	resource	# api/v1.getPREnv
GET /api/v1/stacks/:id/releases	read	resource	# api/v1.listReleases
GET /api/v1/stacks/:id/settings	read	resource	# api/v1.settingsFor
GET /api/v1/stacks/:id/variables	read	resource	# api/v1.getStackVars
GET /api/v1/storage	admin	n/a	# api/v1.adminOnly
GET /api/v1/tiles/:id/backups	read	resource	# api/v1.listBackups
GET /api/v1/tiles/:id/forward	write	resource	# api/v1.forwardTile
GET /apps/:id	read	resource	# web.redirectTile.func1
GET /apps/:id/branches	read	resource	# web/handler/app.Branches
GET /apps/:id/connectors	read	resource	# web/handler/app.Connectors
GET /apps/:id/logs/stream	read	resource	# web/handler/app.LogsStream
GET /apps/:id/metrics	read	resource	# web/handler/app.Metrics
GET /apps/:id/panel	read	resource	# web/handler/app.Panel
GET /apps/:id/panel/content	read	resource	# web/handler/app.PanelContent
GET /apps/:id/panel/header	read	resource	# web/handler/app.PanelHeader
GET /apps/:id/provisions	read	resource	# web/handler/app.Provisions
GET /apps/:id/repos	read	resource	# web/handler/app.Repos
GET /apps/:id/runs/logs/stream	read	resource	# web/handler/app.RunsLogsStream
GET /apps/:id/size	read	resource	# web/handler/app.Size
GET /apps/:id/storage	read	resource	# web/handler/app.StorageFrag
GET /apps/:id/vars	read	resource	# web/handler/app.Vars
GET /apps/:id/vars/value	write	resource	# web/handler/app.VarValue
GET /apps/:id/volume/files	read	resource	# web/handler/app.VolumeFiles
GET /apps/:id/volume/files/download	read	resource	# web/handler/app.VolumeFileDownload
GET /backups/:id/history	read	resource	# web/handler/backups.History
GET /containers	admin	n/a	# web/handler/container.List
GET /containers/:id	admin	n/a	# web/handler/container.Detail
GET /containers/:id/logs	admin	n/a	# web/handler/container.LogsPage
GET /containers/:id/logs/stream	admin	n/a	# web/handler/container.LogsStream
GET /containers/:id/term	admin	n/a	# web/handler/container.TermPage
GET /containers/:id/term/ws	admin	n/a	# web/handler/container.TermWS
GET /containers/node/:id	admin	n/a	# web/handler/container.NodeList
GET /dbs/:id	read	resource	# web.redirectTile.func1
GET /dbs/:id/buckets/:bucket/files	read	resource	# web/handler/db.BucketFiles
GET /dbs/:id/buckets/:bucket/files/download	read	resource	# web/handler/db.BucketFileDownload
GET /dbs/:id/buckets/:bucket/panel	read	resource	# web/handler/db.BucketPanel
GET /dbs/:id/data	read	resource	# web/handler/db.Data
GET /dbs/:id/data/cell	read	resource	# web/handler/db.DataCell
GET /dbs/:id/logs/stream	read	resource	# web/handler/db.LogsStream
GET /dbs/:id/metrics	read	resource	# web/handler/db.Metrics
GET /dbs/:id/panel	read	resource	# web/handler/db.Panel
GET /dbs/:id/panel/content	read	resource	# web/handler/db.PanelContent
GET /dbs/:id/pgdbs/:db/panel	read	resource	# web/handler/db.PGDBPanel
GET /dbs/:id/provisions	read	resource	# web/handler/db.Provisions
GET /dbs/:id/volume/files	read	resource	# web/handler/db.VolumeFiles
GET /dbs/:id/volume/files/download	read	resource	# web/handler/db.VolumeFileDownload
GET /dbs/:id/volume/panel	read	resource	# web/handler/db.VolumePanel
GET /deployments/:id	read	resource	# web/handler/deployment.Detail
GET /deployments/:id/status	read	resource	# web/handler/deployment.Status
GET /deployments/:id/stream	read	resource	# web/handler/deployment.Stream
GET /envs/:id/graph/status	read	resource	# web/handler/project.GraphStatus
GET /envs/:id/logs/stream	read	resource	# web/handler/project.EnvLogsStream
GET /orgs/:slug	read	resource	# web/handler/org.Graph
GET /orgs/:slug/graph/status	read	resource	# web/handler/org.GraphStatus
GET /orgs/:slug/plans	owner	resource	# web/handler/org.Plans
GET /orgs/:slug/plans/:planID	owner	resource	# web/handler/org.OrgPlanView
GET /orgs/:slug/settings	read	resource	# web/handler/org.SettingsIndex
GET /orgs/:slug/settings/backups	read	resource	# web/handler/org.SettingsBackups
GET /orgs/:slug/settings/config	read	resource	# web/handler/org.SettingsConfig
GET /orgs/:slug/settings/config/export	read	resource	# web/handler/org.ExportConfig
GET /orgs/:slug/settings/connectors	read	resource	# web/handler/org.SettingsConnectors
GET /orgs/:slug/settings/connectors/:connectorID/branches	owner	resource	# web/handler/org.ConnectorBranches
GET /orgs/:slug/settings/connectors/:connectorID/file	owner	resource	# web/handler/org.ConnectorFileExists
GET /orgs/:slug/settings/defaults	read	resource	# web/handler/org.SettingsDefaults
GET /orgs/:slug/settings/domains	read	resource	# web/handler/org.SettingsDomains
GET /orgs/:slug/settings/general	read	resource	# web/handler/org.SettingsGeneral
GET /orgs/:slug/settings/invites	owner	resource	# web/handler/org.SettingsInvites
GET /orgs/:slug/settings/members	read	resource	# web/handler/org.SettingsMembers
GET /orgs/:slug/settings/registry	read	resource	# web/handler/org.SettingsRegistry
GET /orgs/:slug/settings/registry/images	read	resource	# web/handler/org.RegistryImages
GET /orgs/:slug/settings/storage	read	resource	# web/handler/org.SettingsStorage
GET /orgs/:slug/settings/variables	read	resource	# web/handler/org.SettingsVariables
GET /orgs/:slug/settings/variables/panel	read	resource	# web/handler/org.VarsPanel
GET /orgs/:slug/settings/variables/value	write	resource	# web/handler/org.OrgVarValue
GET /orgs/:slug/setup/:step	owner	resource	# web/handler/org.Setup
GET /orgs/:slug/setup/config/plan	owner	resource	# web/handler/org.SetupConfigPlan
GET /projects/:id	read	resource	# web/handler/project.RedirectStack
GET /projects/:id/config/plans/:planID	read	resource	# web/handler/project.PlanRedirect
GET /projects/:id/envs/:slug/promote	read	resource	# web/handler/project.PromoteDialogue
GET /projects/:id/envs/compare	read	resource	# web/handler/project.EnvCompare
GET /projects/:id/graph	read	resource	# web/handler/project.RedirectStack
GET /projects/:id/graph/status	read	resource	# web/handler/project.StackGraphStatus
GET /projects/:id/repos	read	resource	# web/handler/project.Repos
GET /projects/:id/staging/:envID	read	resource	# web/handler/project.StagingReview
GET /servers	admin	n/a	# web/handler/server.List
GET /servers/:id	admin	n/a	# web/handler/server.Detail
GET /servers/:id/drain	admin	n/a	# web/handler/server.DrainForm
GET /servers/:id/host	admin	n/a	# web/handler/server.Host
GET /servers/:id/remove	admin	n/a	# web/handler/server.RemoveForm
GET /servers/:id/volumes	admin	n/a	# web/handler/server.Volumes
GET /servers/add	admin	n/a	# web/handler/server.AddNodeForm
GET /servers/move/:id	admin	n/a	# web/handler/server.MoveForm
GET /setup	admin	n/a	# web/handler/org.SetupStart
GET /tiles/:id/backups	read	resource	# web/handler/backups.Panel
`

// ungatedReads are the reads a route-level gate cannot answer, with the reason.
//
// "collection" is a listing that spans orgs. There is no single tenancy to
// resolve before the handler, so the check is a per-row filter inside it —
// a.viewer(c), orgMember(c, row.OrgID) — and stripping it would show every
// user every row. These are NOT unfinished work and must not be "completed".
//
// "asset" is the avatar and org-logo stream. It is authenticated but not
// org-addressed: the path is a storage key, not a resource id, so there is no
// tenancy for a gate to resolve. Any signed-in user could fetch any avatar
// path before the reads were gated and still can — recorded here rather than
// quietly fixed, because narrowing it is a decision about what an avatar URL
// is, not part of moving authorization onto the route.
//
// "deferred" is the read half of service.KindDeferred: the org arrives with
// the request rather than in the path. The GitHub App callback carries an
// opaque state token which IS the credential; the org comes off the row it
// resolves to.
const ungatedReads = `
/avatars/*	asset	# web/avatar.Serve — signed-in, but not org-addressed
/	collection	# web/handler/org.Home
/api/v1/apps	collection	# api/v1.listApps
/api/v1/backup-destinations	collection	# api/v1.listDestinations
/api/v1/dbs	collection	# api/v1.listDBs
/api/v1/domain-resources	collection	# api/v1.listDomainResources
/api/v1/orgs	collection	# api/v1.listOrgs
/api/v1/stacks	collection	# api/v1.listStacks
/graph/status	collection	# web/handler/org.HomeStatus
/search	collection	# web/handler/search.Search
/settings	collection	# web/handler/settings.LegacyRedirect
/settings/github/callback	deferred	# web/handler/settings.GitHubCallback
`

// publicRead is the GET-only half of unauthenticated(): endpoints that answer
// before there is a session at all, or that carry their own credential. The
// health probe and the spec are public by design; the registry token endpoint
// takes a docker basic-auth header, not a cookie; the websocket authenticates
// on its own.
//
// /avatars/* is NOT here. It sits behind auth.RequireAuth() like any other
// page, so it is authenticated, and it is on ungatedReads with the reason.
func publicRead(path string) bool {
	switch path {
	case "/api/health", "/api/openapi.json", "/favicon.ico", "/v2/token", "/ws":
		return true
	}
	return false
}

// TestEveryReadRouteIsGatedOrListed is the read twin of
// TestEveryMutatingRouteNamesAVerb. A new GET either names a verb or says in
// ungatedReads why it cannot.
func TestEveryReadRouteIsGatedOrListed(t *testing.T) {
	listed := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(ungatedReads), "\n") {
		if i := strings.Index(l, "\t"); i > 0 {
			listed[strings.TrimSpace(l[:i])] = true
		}
	}
	captured := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(capturedReadLevels), "\n") {
		captured[strings.TrimSpace(strings.Split(l, "\t")[0])] = true
	}
	var missing []string
	for _, r := range routesAll(t) {
		if r.Method != "GET" || unauthenticated(r.Path) || personal(r.Path) || publicRead(r.Path) {
			continue
		}
		key := "GET " + r.Path
		if captured[key] || listed[r.Path] {
			continue
		}
		missing = append(missing, key+"  ("+r.Name+")")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("GET routes that neither name a verb nor appear in ungatedReads:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// A page above the caller's level must not announce itself: the panel answers
// 404 for the two read verbs that mean "owner only" and "admin only", which is
// what the gate helpers they replaced did. Checked here rather than left to
// the live rig, because it is one `case` away from silently becoming a 403.
func TestOwnerAndAdminReadsAreConcealed(t *testing.T) {
	for _, v := range []service.Verb{service.VerbOrgOwnerRead, service.VerbAdminRead} {
		if !service.KnownVerb(v) {
			t.Fatalf("%s is not in the verb table", v)
		}
	}
	concealed := map[service.Verb]bool{
		service.VerbOrgOwnerRead: true, service.VerbAdminRead: true,
	}
	for _, r := range routesAll(t) {
		if r.Method != "GET" {
			continue
		}
		v := service.Verb(r.Name)
		if !concealed[v] {
			continue
		}
		if lvl := service.LevelOf(v); lvl != service.LevelOwner && lvl != service.LevelAdmin {
			t.Errorf("GET %s names %s, which is meant to conceal but sits at level %v",
				r.Path, v, lvl)
		}
	}
}

// TestReadVerbAgreesWithCapturedLevel is the read twin of
// TestRouteVerbAgreesWithCapturedLevel: the verb a read route names has to sit
// at the level that read enforced before it was gated.
func TestReadVerbAgreesWithCapturedLevel(t *testing.T) {
	want := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(capturedReadLevels), "\n") {
		f := strings.Split(l, "\t")
		want[strings.TrimSpace(f[0])] = strings.TrimSpace(f[1])
	}
	for _, r := range routesAll(t) {
		if r.Method != "GET" {
			continue
		}
		lvl, ok := want["GET "+r.Path]
		if !ok {
			continue
		}
		levels := map[service.Level]string{
			service.LevelRead: "read", service.LevelWrite: "write",
			service.LevelOwner: "owner", service.LevelAdmin: "admin",
		}
		v := service.Verb(r.Name)
		if !service.KnownVerb(v) {
			t.Errorf("GET %s is captured at level %s but names %q, which is not a verb",
				r.Path, lvl, r.Name)
			continue
		}
		if got := levels[service.LevelOf(v)]; got != lvl {
			t.Errorf("GET %s names %s (level %s) but enforced %s before it was gated.\n"+
				"Either the verb is wrong, or this is a deliberate change — in which case\n"+
				"edit capturedReadLevels and say what decided it.", r.Path, v, got, lvl)
		}
	}
}
