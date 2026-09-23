package web_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
)

// The captured-level table — point 18's second safety net.
//
// TestEveryMutatingRouteNamesAVerb (routeverb_test.go) proves a route names a
// verb. It cannot prove the verb is the RIGHT one: VerbStackWrite and
// VerbOrgWrite are both "a known verb", so putting the member-level one on an
// owner-level route passes that test, passes go vet, and leaves the endpoint
// open one rung. That is the silent failure point 18 is dangerous for.
//
// So before the gate helpers are deleted, what they enforce today is captured
// here as data. The helpers are the only record of it; once they are gone
// this table is. Every route that grows a verb has to agree with the level
// recorded below, and a DELIBERATE change — the "higher level wins" cases
// decided in points 7, 10 and 11 — is an explicit edit here naming the point
// that decided it, never a silent move.
//
// How the table was built (docs/plans/service-extraction/07-point-18-plan.md):
// an AST walk of the handler tree resolving, per handler, the transitive set
// of gate helpers its body reaches, joined to the registered routes. Levels
// that a call site decides by argument (requireTile(c, id, write) and the
// requireVerb(..., service.VerbX, ...) family) are resolved from the argument,
// with verb levels read out of service/access.go so this and the product
// cannot disagree. 266 of the 271 mutating routes resolved; the other five
// were the org-home graph routes, which turned out to write the caller's own
// saved layout (repo.ScopeUser) and moved to personal().
//
// Columns: route, level, and WHICH ORG the level is checked against.
//
// The third column is not decoration. CanWrite and the site-wide
// ReadOnlyGuard check the ACTIVE cookie org; CanWriteOrg, RequireOrgWrite and
// RequireStackAccess check the org that owns the addressed resource. Both are
// "write" on the ladder and they are not the same check — someone who owns
// org A and only views org B carries role owner while acting on B's tiles.
// Without this column a route could move from resource-org to active-org
// checking and every test here would still pass.
//
// Today every one of these checks the resource's org (the admin ones are not
// org-addressed at all, hence n/a). That is the invariant point 18 must not
// break, so it is recorded rather than assumed.
//
// Levels are the ladder in service/access.go: read < write < owner < admin.
const capturedLevels = `
DELETE /api/v1/apps/:id	write	resource	# api/v1.deleteApp
DELETE /api/v1/backup-destinations/:id	write	resource	# api/v1.deleteDestination
DELETE /api/v1/backups/:id	write	resource	# api/v1.deleteBackup
DELETE /api/v1/dbs/:id	write	resource	# api/v1.deleteDB
DELETE /api/v1/domain-resources/:id	write	resource	# api/v1.deleteDomainResource
DELETE /api/v1/domains/:id	write	resource	# api/v1.deleteDomain
DELETE /api/v1/envs/:id	write	resource	# api/v1.deleteEnv
DELETE /api/v1/orgs/:id/invites/:invite	owner	resource	# api/v1.deleteInvite
DELETE /api/v1/orgs/:id/members/:user	owner	resource	# api/v1.removeMember
DELETE /api/v1/orgs/:id/registry/credentials/:cred	owner	resource	# api/v1.deleteRegistryCredential
DELETE /api/v1/orgs/:id/registry/images/:name/tags/:tag	owner	resource	# api/v1.deleteRegistryTag
DELETE /api/v1/provisions/:id	write	resource	# api/v1.deleteProvision
DELETE /api/v1/proxy/entries/:name	admin	n/a	# api/v1.adminOnly
DELETE /api/v1/registries/:id	admin	n/a	# api/v1.adminOnly
DELETE /api/v1/stacks/:id	write	resource	# api/v1.deleteStack
DELETE /api/v1/storage/:id	admin	n/a	# api/v1.adminOnly
DELETE /api/v1/storage/paths/:id	admin	n/a	# api/v1.adminOnly
DELETE /api/v1/volumes/:id	write	resource	# api/v1.deleteVolume
PATCH /api/v1/apps/:id	write	resource	# api/v1.patchApp
PATCH /api/v1/backup-destinations/:id	admin	n/a	# api/v1.patchDestination
PATCH /api/v1/backups/:id	write	resource	# api/v1.patchBackup
PATCH /api/v1/dbs/:id	write	resource	# api/v1.patchDB
PATCH /api/v1/domain-resources/:id	write	resource	# api/v1.patchDomainResource
PATCH /api/v1/domains/:id	write	resource	# api/v1.patchDomain
PATCH /api/v1/envs/:id	write	resource	# api/v1.patchEnv
PATCH /api/v1/envs/:id/settings	owner	resource	# api/v1.patchSettingsFor
PATCH /api/v1/orgs/:id/members/:user	owner	resource	# api/v1.setMemberRole
PATCH /api/v1/orgs/:id/settings	owner	resource	# api/v1.patchSettingsFor
PATCH /api/v1/registries/:id	admin	n/a	# api/v1.adminOnly
PATCH /api/v1/settings	admin	n/a	# api/v1.patchSettingsFor
PATCH /api/v1/stacks/:id	write	resource	# api/v1.patchStack
PATCH /api/v1/stacks/:id/settings	owner	resource	# api/v1.patchSettingsFor
POST /admin/backups/destinations	admin	n/a	# web/handler/settings.CreateDestination
POST /admin/backups/destinations/:destID/delete	admin	n/a	# web/handler/settings.DeleteDestination
POST /admin/backups/destinations/:destID/shared	admin	n/a	# web/handler/settings.ToggleDestinationShared
POST /admin/backups/panel	admin	n/a	# web/handler/settings.SavePanelBackup
POST /admin/backups/panel/delete	admin	n/a	# web/handler/settings.DeletePanelBackup
POST /admin/backups/panel/run	admin	n/a	# web/handler/settings.RunPanelBackup
POST /admin/cleanup	admin	n/a	# web/handler/settings.ToggleCleanup
POST /admin/dns	admin	n/a	# web/handler/settings.SaveDNS
POST /admin/imagewatch	admin	n/a	# web/handler/settings.SaveImageWatch
POST /admin/proxy/entry	admin	n/a	# web/handler/settings.SaveProxyEntry
POST /admin/proxy/entry/delete	admin	n/a	# web/handler/settings.DeleteProxyEntry
POST /admin/proxy/override	admin	n/a	# web/handler/settings.SaveProxyOverride
POST /admin/proxy/trusted	admin	n/a	# web/handler/settings.SaveTrustedProxies
POST /admin/registries	admin	n/a	# web/handler/settings.CreateRegistry
POST /admin/registries/:id/delete	admin	n/a	# web/handler/settings.DeleteRegistry
POST /admin/registry/domain	admin	n/a	# web/handler/settings.SetRegistryDomain
POST /admin/update	admin	n/a	# web/handler/settings.RunUpdate
POST /admin/users/:id/admin	admin	n/a	# web/handler/settings.ToggleUserAdmin
POST /admin/users/:id/toggle	admin	n/a	# web/handler/settings.ToggleUserActive
POST /api/v1/apps/:id/cron/toggle	write	resource	# api/v1.toggleCron
POST /api/v1/apps/:id/deploy	write	resource	# api/v1.deployApp
POST /api/v1/apps/:id/domains	write	resource	# api/v1.createDomain
POST /api/v1/apps/:id/domains/auto	write	resource	# api/v1.createAutoDomain
POST /api/v1/apps/:id/provision	write	resource	# api/v1.provisionApp
POST /api/v1/apps/:id/provisions/:pid/detach	write	resource	# api/v1.detachProvision
POST /api/v1/apps/:id/provisions/attach	write	resource	# api/v1.attachProvision
POST /api/v1/apps/:id/restart	write	resource	# api/v1.restartApp
POST /api/v1/apps/:id/rollback	write	resource	# api/v1.rollbackApp
POST /api/v1/apps/:id/run	write	resource	# api/v1.runApp
POST /api/v1/apps/:id/runs/:run/stop	write	resource	# api/v1.stopRun
POST /api/v1/apps/:id/stop	write	resource	# api/v1.stopApp
POST /api/v1/apps/:id/volumes	write	resource	# api/v1.createVolume
POST /api/v1/backup-destinations	write	resource	# api/v1.createDestination
POST /api/v1/backups/:id/restore	write	resource	# api/v1.restoreBackup
POST /api/v1/backups/:id/run	write	resource	# api/v1.runBackup
POST /api/v1/config/plans/:id/approve	write	resource	# api/v1.approvePlan
POST /api/v1/config/plans/:id/reject	write	resource	# api/v1.rejectPlan
POST /api/v1/dbs/:id/provisions	write	resource	# api/v1.createInstanceProvision
POST /api/v1/deployments/:id/cancel	write	resource	# api/v1.cancelDeployment
POST /api/v1/domain-resources	write	resource	# api/v1.createDomainResource
POST /api/v1/envs/:id/copy	write	resource	# api/v1.copyEnv
POST /api/v1/envs/:id/reset	write	resource	# api/v1.resetEnv
POST /api/v1/org-config/plans/:id/approve	owner	resource	# api/v1.approveOrgPlan
POST /api/v1/org-config/plans/:id/reject	owner	resource	# api/v1.rejectOrgPlan
POST /api/v1/orgs/:id/config/plan	owner	resource	# api/v1.planOrg
POST /api/v1/orgs/:id/config/plan-preview	owner	resource	# api/v1.previewOrgPlan
POST /api/v1/orgs/:id/invites	owner	resource	# api/v1.createInvite
POST /api/v1/orgs/:id/members	owner	resource	# api/v1.addMember
POST /api/v1/orgs/:id/registry/credentials	owner	resource	# api/v1.createRegistryCredential
POST /api/v1/provisions/:id/fork	write	resource	# api/v1.forkProvision
POST /api/v1/provisions/:id/public	write	resource	# api/v1.setProvisionPublic
POST /api/v1/registries	admin	n/a	# api/v1.adminOnly
POST /api/v1/resolve	read	resource	# api/v1.resolvePath
POST /api/v1/stacks	write	resource	# api/v1.createStack
POST /api/v1/stacks/:id/apps	write	resource	# api/v1.createApp
POST /api/v1/stacks/:id/config/plan	write	resource	# api/v1.planStack
POST /api/v1/stacks/:id/config/plan-preview	write	resource	# api/v1.previewPlan
POST /api/v1/stacks/:id/dbs	write	resource	# api/v1.createDB
POST /api/v1/stacks/:id/envs	write	resource	# api/v1.createEnv
POST /api/v1/stacks/:id/envs/:slug/promote	write	resource	# api/v1.promote
POST /api/v1/storage	admin	n/a	# api/v1.adminOnly
POST /api/v1/storage/:id/paths	admin	n/a	# api/v1.adminOnly
POST /api/v1/storage/:id/probe	admin	n/a	# api/v1.adminOnly
POST /api/v1/tiles/:id/backups	write	resource	# api/v1.createBackup
POST /apps/:id/attach	write	resource	# web/handler/app.Attach
POST /apps/:id/cron/toggle	write	resource	# web/handler/app.ToggleCron
POST /apps/:id/delete	write	resource	# web/handler/app.Delete
POST /apps/:id/deploy	write	resource	# web/handler/deployment.Deploy
POST /apps/:id/domains	write	resource	# web/handler/app.CreateDomain
POST /apps/:id/domains/:domainID/cert	write	resource	# web/handler/app.SetDomainCert
POST /apps/:id/domains/:domainID/delete	write	resource	# web/handler/app.DeleteDomain
POST /apps/:id/domains/:domainID/https	write	resource	# web/handler/app.ToggleDomainHTTPS
POST /apps/:id/domains/auto	write	resource	# web/handler/app.CreateAutoDomain
POST /apps/:id/env	write	resource	# web/handler/app.SaveEnv
POST /apps/:id/provision	write	resource	# web/handler/app.Provision
POST /apps/:id/provisions/:pid/detach	write	resource	# web/handler/app.DetachProvision
POST /apps/:id/provisions/attach	write	resource	# web/handler/app.AttachProvision
POST /apps/:id/restart	write	resource	# web/handler/app.Restart
POST /apps/:id/rollback	write	resource	# web/handler/deployment.Rollback
POST /apps/:id/run	write	resource	# web/handler/app.RunNow
POST /apps/:id/runs/:run/stop	write	resource	# web/handler/app.StopRun
POST /apps/:id/settings	write	resource	# web/handler/app.SaveSettings
POST /apps/:id/stop	write	resource	# web/handler/app.Stop
POST /apps/:id/storage	write	resource	# web/handler/app.AttachStorage
POST /apps/:id/storage/detach	write	resource	# web/handler/app.DetachStorage
POST /apps/:id/vars/delete	write	resource	# web/handler/app.DeleteVar
POST /apps/:id/vars/secret	write	resource	# web/handler/app.SaveSecretVar
POST /apps/:id/volume/files/delete	write	resource	# web/handler/app.VolumeFileDelete
POST /apps/:id/volume/files/upload	write	resource	# web/handler/app.VolumeFileUpload
POST /backups/:id/delete	write	resource	# web/handler/backups.Delete
POST /backups/:id/restore	write	resource	# web/handler/backups.Restore
POST /backups/:id/run	write	resource	# web/handler/backups.Run
POST /backups/:id/save	write	resource	# web/handler/backups.Save
POST /containers/:id/remove	admin	n/a	# web/handler/container.Remove
POST /containers/:id/start	admin	n/a	# web/handler/container.Start
POST /containers/:id/stop	admin	n/a	# web/handler/container.Stop
POST /dbs/:id/buckets/:bucket/files/delete	write	resource	# web/handler/db.BucketFileDelete
POST /dbs/:id/buckets/:bucket/files/upload	write	resource	# web/handler/db.BucketFileUpload
POST /dbs/:id/data/delete	write	resource	# web/handler/db.DataDelete
POST /dbs/:id/data/insert	write	resource	# web/handler/db.DataInsert
POST /dbs/:id/data/update	write	resource	# web/handler/db.DataUpdate
POST /dbs/:id/delete	write	resource	# web/handler/db.Delete
POST /dbs/:id/deploy	write	resource	# web/handler/db.Deploy
POST /dbs/:id/domains	write	resource	# web/handler/db.CreateDomain
POST /dbs/:id/domains/:domainID/delete	write	resource	# web/handler/db.DeleteDomain
POST /dbs/:id/port	write	resource	# web/handler/db.SetPort
POST /dbs/:id/provisions/drop	write	resource	# web/handler/db.DropProvision
POST /dbs/:id/provisions/fork	write	resource	# web/handler/db.ForkProvision
POST /dbs/:id/provisions/public	write	resource	# web/handler/db.SetProvisionPublic
POST /dbs/:id/scope	write	resource	# web/handler/db.SetScope
POST /dbs/:id/start	write	resource	# web/handler/db.Start
POST /dbs/:id/stop	write	resource	# web/handler/db.Stop
POST /dbs/:id/volume/files/delete	write	resource	# web/handler/db.VolumeFileDelete
POST /dbs/:id/volume/files/upload	write	resource	# web/handler/db.VolumeFileUpload
POST /deployments/:id/cancel	write	resource	# web/handler/deployment.Cancel
POST /envs/:id/color	write	resource	# web/handler/project.SaveEnvColor
POST /envs/:id/config	write	resource	# web/handler/project.SaveEnvConfig
POST /envs/:id/copy	write	resource	# web/handler/project.CopyEnv
POST /envs/:id/delete	write	resource	# web/handler/project.DeleteEnvironment
POST /envs/:id/graph/annotations	write	resource	# web/handler/project.SaveEnvAnnotation
POST /envs/:id/graph/annotations/delete	write	resource	# web/handler/project.DeleteEnvAnnotation
POST /envs/:id/graph/groups	write	resource	# web/handler/project.SaveEnvGraphGroup
POST /envs/:id/graph/groups/delete	write	resource	# web/handler/project.DeleteEnvGraphGroup
POST /envs/:id/graph/positions	write	resource	# web/handler/project.SaveNodePosition
POST /envs/:id/graph/positions/reset	write	resource	# web/handler/project.ResetNodePositions
POST /envs/:id/intended	write	resource	# web/handler/project.MarkIntended
POST /envs/:id/reset	write	resource	# web/handler/project.ResetEnvironment
POST /envs/:id/settings	write	resource	# web/handler/project.SaveEnvSettings
POST /envs/:id/vars	write	resource	# web/handler/project.SaveEnvVar
POST /envs/:id/vars/delete	write	resource	# web/handler/project.DeleteEnvVar
POST /orgs	admin	n/a	# web/handler/org.Create
POST /orgs/:slug/delete	owner	resource	# web/handler/org.Delete
POST /orgs/:slug/graph/annotations	write	resource	# web/handler/org.SaveAnnotation
POST /orgs/:slug/graph/annotations/delete	write	resource	# web/handler/org.DeleteAnnotation
POST /orgs/:slug/graph/groups	write	resource	# web/handler/org.SaveGraphGroup
POST /orgs/:slug/graph/groups/delete	write	resource	# web/handler/org.DeleteGraphGroup
POST /orgs/:slug/graph/positions	write	resource	# web/handler/org.SaveNodePosition
POST /orgs/:slug/graph/positions/reset	write	resource	# web/handler/org.ResetNodePositions
POST /orgs/:slug/invites	owner	resource	# web/handler/org.CreateInvite
POST /orgs/:slug/invites/:inviteID/delete	owner	resource	# web/handler/org.DeleteInvite
POST /orgs/:slug/invites/:inviteID/reinvite	owner	resource	# web/handler/org.ReinviteMember
POST /orgs/:slug/invites/:inviteID/resend	owner	resource	# web/handler/org.ResendInvite
POST /orgs/:slug/members	owner	resource	# web/handler/org.AddMember
POST /orgs/:slug/members/:userID/remove	owner	resource	# web/handler/org.RemoveMember
POST /orgs/:slug/members/:userID/role	owner	resource	# web/handler/org.SetMemberRole
POST /orgs/:slug/plans/:planID/approve	owner	resource	# web/handler/org.ApproveOrgPlan
POST /orgs/:slug/plans/:planID/inputs	owner	resource	# web/handler/org.SetPlanInput
POST /orgs/:slug/plans/:planID/reject	owner	resource	# web/handler/org.RejectOrgPlan
POST /orgs/:slug/rename	owner	resource	# web/handler/org.Rename
POST /orgs/:slug/settings/backups/destinations	write	resource	# web/handler/settings.CreateDestination
POST /orgs/:slug/settings/backups/destinations/:destID/delete	write	resource	# web/handler/settings.DeleteDestination
POST /orgs/:slug/settings/config	owner	resource	# web/handler/org.SaveOrgConfig
POST /orgs/:slug/settings/connectors/:connectorID/delete	write	resource	# web/handler/settings.DeleteConnector
POST /orgs/:slug/settings/connectors/github	write	resource	# web/handler/settings.GitHubConnect
POST /orgs/:slug/settings/defaults	owner	resource	# web/handler/org.SaveDefaults
POST /orgs/:slug/settings/domains	owner	resource	# web/handler/org.SaveOrgDomain (was write; raised by decision #1 in 06-points-18-20.md)
POST /orgs/:slug/settings/domains/delete	owner	resource	# web/handler/org.DeleteOrgDomain
POST /orgs/:slug/settings/env-colors	owner	resource	# web/handler/org.SaveEnvColor
POST /orgs/:slug/settings/registry/credentials	owner	resource	# web/handler/org.CreateRegistryCredential
POST /orgs/:slug/settings/registry/credentials/delete	owner	resource	# web/handler/org.DeleteRegistryCredential
POST /orgs/:slug/settings/registry/tags/delete	owner	resource	# web/handler/org.DeleteRegistryTag
POST /orgs/:slug/settings/vars	write	resource	# web/handler/org.SaveOrgVar
POST /orgs/:slug/settings/vars/delete	write	resource	# web/handler/org.DeleteOrgVar
POST /orgs/:slug/setup/config	owner	resource	# web/handler/org.SaveOrgConfig
POST /orgs/:slug/setup/config/plan/:planID/approve	owner	resource	# web/handler/org.SetupApprovePlan
POST /orgs/:slug/setup/config/plan/:planID/inputs	owner	resource	# web/handler/org.SetPlanInput
POST /orgs/:slug/setup/config/plan/:planID/reject	owner	resource	# web/handler/org.SetupRejectPlan
POST /orgs/:slug/setup/connector	owner	resource	# web/handler/settings.GitHubConnect
POST /orgs/:slug/setup/domain	owner	resource	# web/handler/org.SaveOrgDomain
POST /orgs/:slug/setup/done	owner	resource	# web/handler/org.SetupDone
POST /orgs/:slug/setup/mode	owner	resource	# web/handler/org.SetupMode
POST /orgs/:slug/setup/name	owner	resource	# web/handler/org.Rename
POST /orgs/:slug/setup/team/invites/:inviteID/delete	owner	resource	# web/handler/org.DeleteInvite
POST /orgs/:slug/setup/team/invites/:inviteID/reinvite	owner	resource	# web/handler/org.ReinviteMember
POST /orgs/:slug/setup/team/invites/:inviteID/resend	owner	resource	# web/handler/org.ResendInvite
POST /orgs/:slug/setup/team/members	owner	resource	# web/handler/org.AddMember
POST /orgs/:slug/setup/team/members/:userID/remove	owner	resource	# web/handler/org.RemoveMember
POST /orgs/:slug/setup/team/members/:userID/role	owner	resource	# web/handler/org.SetMemberRole
POST /orgs/stacks/:id/move	write	resource	# web/handler/org.MoveStack
POST /projects	write	resource	# web/handler/project.Create
POST /projects/:id/apps	write	resource	# web/handler/project.CreateTile
POST /projects/:id/config	write	resource	# web/handler/project.SaveConfigBinding
POST /projects/:id/config/plan	write	resource	# web/handler/project.PlanNow
POST /projects/:id/config/plans/:planID/approve	write	resource	# web/handler/project.ApprovePlan
POST /projects/:id/config/plans/:planID/inputs	write	resource	# web/handler/project.SetPlanInput
POST /projects/:id/config/plans/:planID/reject	write	resource	# web/handler/project.RejectPlan
POST /projects/:id/dbs	write	resource	# web/handler/project.CreateDB
POST /projects/:id/delete	write	resource	# web/handler/project.Delete
POST /projects/:id/domain-resources	write	resource	# web/handler/project.SaveStackDomain
POST /projects/:id/domain-resources/delete	write	resource	# web/handler/project.DeleteStackDomain
POST /projects/:id/envs	write	resource	# web/handler/project.CreateEnvironment
POST /projects/:id/envs/:slug/promote	write	resource	# web/handler/project.PromoteCommit
POST /projects/:id/graph/annotations	write	resource	# web/handler/project.SaveStackAnnotation
POST /projects/:id/graph/annotations/delete	write	resource	# web/handler/project.DeleteStackAnnotation
POST /projects/:id/graph/groups	write	resource	# web/handler/project.SaveStackGraphGroup
POST /projects/:id/graph/groups/delete	write	resource	# web/handler/project.DeleteStackGraphGroup
POST /projects/:id/graph/positions	write	resource	# web/handler/project.SaveStackNodePosition
POST /projects/:id/graph/positions/reset	write	resource	# web/handler/project.ResetStackNodePositions
POST /projects/:id/links	write	resource	# web/handler/project.MintStackLink
POST /projects/:id/links/revoke	write	resource	# web/handler/project.RevokeStackLink
POST /projects/:id/prenv	write	resource	# web/handler/project.SavePREnv
POST /projects/:id/prenv/rotate	write	resource	# web/handler/project.RotatePRSecret
POST /projects/:id/settings	write	resource	# web/handler/project.SaveSettings
POST /projects/:id/staging/:envID/apply	write	resource	# web/handler/project.StagingApply
POST /projects/:id/staging/:envID/changes/:changeID/discard	write	resource	# web/handler/project.StagingDiscardOne
POST /projects/:id/staging/:envID/discard	write	resource	# web/handler/project.StagingDiscard
POST /projects/:id/vars	write	resource	# web/handler/project.SaveStackVar
POST /projects/:id/vars/delete	write	resource	# web/handler/project.DeleteStackVar
POST /servers/:id/activate	admin	n/a	# web/handler/server.Activate
POST /servers/:id/domains	admin	n/a	# web/handler/server.CreateDomainResource
POST /servers/:id/domains/delete	admin	n/a	# web/handler/server.DeleteDomainResource
POST /servers/:id/drain	admin	n/a	# web/handler/server.Drain
POST /servers/:id/group	admin	n/a	# web/handler/server.SaveGroup
POST /servers/:id/join-key	admin	n/a	# web/handler/server.NewJoinKey
POST /servers/:id/remove	admin	n/a	# web/handler/server.Remove
POST /servers/:id/settings	admin	n/a	# web/handler/server.SaveSettings
POST /servers/:id/storage	admin	n/a	# web/handler/server.CreateStorage
POST /servers/:id/storage/delete	admin	n/a	# web/handler/server.DeleteStorage
POST /servers/:id/storage/paths	admin	n/a	# web/handler/server.CreateStoragePath
POST /servers/:id/storage/paths/delete	admin	n/a	# web/handler/server.DeleteStoragePath
POST /servers/:id/storage/probe	admin	n/a	# web/handler/server.ProbeStorage
POST /servers/:id/volumes	admin	n/a	# web/handler/server.CreateVolume
POST /servers/:id/volumes/delete	admin	n/a	# web/handler/server.DeleteVolume
POST /servers/add	admin	n/a	# web/handler/server.AddNode
POST /servers/move/:id	admin	n/a	# web/handler/server.Move
POST /tiles/:id/backups	write	resource	# web/handler/backups.Create
PUT /api/v1/apps/:id/variables	write	resource	# api/v1.putVars
PUT /api/v1/envs/:id/variables	write	resource	# api/v1.putEnvVars
PUT /api/v1/orgs/:id/variables	write	resource	# api/v1.putOrgVars
PUT /api/v1/proxy/entries/:name	admin	n/a	# api/v1.adminOnly
PUT /api/v1/proxy/static-override	admin	n/a	# api/v1.adminOnly
PUT /api/v1/stacks/:id/pr-envs	write	resource	# api/v1.putPREnv
PUT /api/v1/stacks/:id/variables	write	resource	# api/v1.putStackVars
`

// captured parses the table into method+path -> level.
func captured(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(capturedLevels), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			t.Fatalf("malformed row: %q", line)
		}
		out[strings.TrimSpace(f[0])] = strings.TrimSpace(f[1]) + "\t" + strings.TrimSpace(f[2])
	}
	return out
}

// Every route that owes a verb has a captured level, and nothing is captured
// that is no longer a route. A new mutating route has to record what it
// enforces before point 18 can give it a verb.
func TestEveryMutatingRouteHasACapturedLevel(t *testing.T) {
	have := captured(t)
	seen := map[string]bool{}
	var missing []string
	for _, r := range routes(t) {
		key := r.Method + " " + r.Path
		seen[key] = true
		if _, ok := have[key]; !ok {
			missing = append(missing, key)
		}
	}
	var stale []string
	for key := range have {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("%d mutating route(s) have no captured level. Record what the route\n"+
			"enforces TODAY before giving it a verb:\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("%d captured level(s) name no route; remove them:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}

// The one that matters: a route that names a verb must name one whose level
// is what the route enforced before. Empty until point 18 starts putting
// verbs on routes, and it grows a case with every one of them.
func TestRouteVerbAgreesWithCapturedLevel(t *testing.T) {
	have := captured(t)
	levels := map[service.Level]string{
		service.LevelRead: "read", service.LevelWrite: "write",
		service.LevelOwner: "owner", service.LevelAdmin: "admin",
	}
	checked := 0
	for _, r := range routes(t) {
		v := service.Verb(r.Name)
		if !service.KnownVerb(v) {
			continue // still gated in the handler body; the other test owns it
		}
		key := r.Method + " " + r.Path
		want, ok := have[key]
		if !ok {
			continue // the coverage test above reports it
		}
		checked++
		wantLevel, wantOrg, _ := strings.Cut(want, "\t")
		_ = wantOrg // TenancyOf (step 2b) is what will assert the org half
		want = wantLevel
		if got := levels[service.LevelOf(v)]; got != want {
			t.Errorf("%s names %s (level %s) but enforced %s before point 18.\n"+
				"Either the verb is wrong, or this is a deliberate change — in which case\n"+
				"edit capturedLevels and say which point decided it.", key, v, got, want)
		}
	}
	t.Logf("%d route(s) verb-gated so far, of %d owed", checked, len(have))
}
