package web_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Point 19's net: a handler must not read the store.
//
// The rule the dev set is narrower than "no repo import", and deliberately so.
// Handlers keep repo.X as view types — the templ views take them, and changing
// that is a type migration across 150 handler files and 39 views, measured and
// rejected. What is banned is the CALL: h.store.GetX / a.store.ListY inside a
// handler package, because a read chosen by a handler is a read that cannot be
// made the same way twice. Two pages that assemble "the org and its stacks"
// out of separate store calls drift, and the second one is where a filter gets
// forgotten.
//
// This is the same shape as gatefree_test.go, for the same reason: the failure
// is silent. A read left behind compiles, passes every test, and renders.
//
// stillStoreReading is the worklist. It shrinks; it does not grow. A new
// handler that reaches for the store fails here on the first run.

// storeReads finds, per "pkg.Func", the store methods its body calls —
// transitively through same-package helpers, which is where the panel's
// loaders live.
func storeReads(t *testing.T, root string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	type fn struct {
		pkg   string
		store []string
		calls []string
	}
	byKey := map[string]*fn{}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".go") ||
			strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, "_templ.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return nil
		}
		pkg := filepath.Dir(p)
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			key := pkg + "." + fd.Name.Name
			e := byKey[key]
			if e == nil {
				e = &fn{pkg: pkg}
				byKey[key] = e
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				ce, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := ce.Fun.(*ast.SelectorExpr)
				if !ok {
					// A bare call is this package's own: follow it, so a
					// handler that reads through a local helper still counts.
					if id, ok := ce.Fun.(*ast.Ident); ok {
						e.calls = append(e.calls, id.Name)
					}
					return true
				}
				// x.store.Method(...) — the thing being banned.
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "store" {
					e.store = append(e.store, sel.Sel.Name)
					return true
				}
				// h.helper(...) on the receiver is a local edge; anything
				// deeper (h.tiles.Create) belongs to another package.
				if _, ok := sel.X.(*ast.Ident); ok {
					e.calls = append(e.calls, sel.Sel.Name)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	out := map[string][]string{}
	var reach func(key string, seen map[string]bool) map[string]bool
	reach = func(key string, seen map[string]bool) map[string]bool {
		got := map[string]bool{}
		if seen[key] {
			return got
		}
		seen[key] = true
		e := byKey[key]
		if e == nil {
			return got
		}
		for _, m := range e.store {
			got[m] = true
		}
		for _, c := range e.calls {
			for k := range reach(e.pkg+"."+c, seen) {
				got[k] = true
			}
		}
		return got
	}
	for key := range byKey {
		if g := reach(key, map[string]bool{}); len(g) > 0 {
			names := make([]string, 0, len(g))
			for n := range g {
				names = append(names, n)
			}
			sort.Strings(names)
			out[key] = names
		}
	}
	return out
}

// handlerPkg reports whether a scanned path is a handler package — the ones
// point 19 is about. The middleware and the router itself are not: the gate
// resolves tenancy from the store by design, and registration reads nothing.
func handlerPkg(key string) bool {
	switch {
	case strings.Contains(key, "/handler/"):
		return true
	case strings.Contains(key, "/api/v1"):
		return true
	}
	return false
}

func TestNoHandlerReadsTheStore(t *testing.T) {
	reads := storeReads(t, "../..")
	known := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(stillStoreReading), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			known[strings.TrimSpace(strings.SplitN(l, " -> ", 2)[0])] = true
		}
	}
	var offenders, stale []string
	seen := map[string]bool{}
	for key, methods := range reads {
		if !handlerPkg(key) {
			continue
		}
		short := strings.TrimPrefix(key, "../../")
		short = strings.TrimPrefix(short, "internal/stackrd/handlers/")
		seen[short] = true
		if !known[short] {
			offenders = append(offenders, short+" -> "+strings.Join(methods, ", "))
		}
	}
	for k := range known {
		if !seen[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(offenders)
	sort.Strings(stale)
	if len(offenders) > 0 {
		t.Errorf("%d handler(s) read the store and are not on stillStoreReading.\n"+
			"A handler asks a service for what it renders; a read it makes itself is a read\n"+
			"nothing else can make the same way. Move it, or add it with the reason:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("%d entr(ies) on stillStoreReading no longer read the store; remove them:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
	t.Logf("%d handler function(s) still reading the store", len(seen))
}

// stillStoreReading is point 19's worklist: handler functions whose body still
// reaches the store, directly or through a helper in the same package.
//
// Every entry is work not yet done, and the list only shrinks. It was seeded
// from the scan rather than typed, so it is an accurate picture of the
// starting point and not a wish.
//
// Read it as functions, not call sites: a page handler that calls h.loadStack
// is here because loadStack reads, so moving one loader clears several rows.
// 446 functions remain. Domain slices done: environments and variables, then
// stacks and tiles.
//
// The order of work, decided with the dev: components/metrics.templ first (a
// TEMPLATE reading the store is the worst of them), then the domains that
// already have a service to move into, then page assembly — which becomes one
// view method per page family, never one forwarder per store method.
const stillStoreReading = `
handlers/api/handler/health.Health -> Health
handlers/api/v1.KeyAuth -> GetAPIKeyByHash, GetUserByID, ListOrgsForUser
handlers/api/v1.Register -> GetServer
handlers/api/v1.approveOrgPlan -> GetOrgConfigPlan
handlers/api/v1.approvePlan -> GetConfigPlan
handlers/api/v1.attachProvision -> GetProvision
handlers/api/v1.cancelDeployment -> GetDeployment
handlers/api/v1.checkConnector -> GetConnector
handlers/api/v1.createAutoDomain -> ListDomainsByTile
handlers/api/v1.createDB -> GetOrg
handlers/api/v1.createDomainResource -> GetOrg, GetOrgBySlug
handlers/api/v1.createInvite -> CreateInvite
handlers/api/v1.createRegistry -> CreateRegistry
handlers/api/v1.createRegistryCredential -> GetManagedRegistry
handlers/api/v1.createStack -> GetOrg, GetOrgMember, ListOrgs
handlers/api/v1.createStorage -> GetOrg, GetOrgBySlug, ListStoragePaths
handlers/api/v1.createStoragePath -> GetStorage
handlers/api/v1.deleteBackup -> DeleteBackup, GetBackup
handlers/api/v1.deleteDestination -> GetBackupDestination, ListOrgs
handlers/api/v1.deleteDomain -> GetDomain
handlers/api/v1.deleteDomainResource -> GetOrg, GetOrgBySlug
handlers/api/v1.deleteInvite -> DeleteInvite, GetInvite
handlers/api/v1.deleteProvision -> GetProvision
handlers/api/v1.deleteRegistry -> DeleteRegistry, GetRegistry
handlers/api/v1.deleteRegistryTag -> GetManagedRegistry
handlers/api/v1.deleteStorage -> GetOrg, GetOrgBySlug, GetStorage
handlers/api/v1.deleteStoragePath -> DeleteStoragePath, GetStorage, GetStoragePath
handlers/api/v1.envByPath -> GetOrgBySlug
handlers/api/v1.forkProvision -> GetProvision
handlers/api/v1.gate -> GetOrg
handlers/api/v1.getDB -> GetOrg
handlers/api/v1.getDeployment -> GetDeployment
handlers/api/v1.getDeploymentLogs -> GetDeployment
handlers/api/v1.getOrgPlan -> GetOrgConfigPlan
handlers/api/v1.getPlan -> GetConfigPlan
handlers/api/v1.getRestore -> GetBackup
handlers/api/v1.infraPath -> GetOrg
handlers/api/v1.listAppProvisions -> ListProvisionsByConsumer
handlers/api/v1.listAppResources -> BindingsForConsumer, GetResource, ListOutputs
handlers/api/v1.listApps -> ListOrgs
handlers/api/v1.listBackupRuns -> GetBackup, ListBackupRuns
handlers/api/v1.listBackups -> ListBackupsByTile
handlers/api/v1.listDBs -> GetOrg, ListOrgs
handlers/api/v1.listDeployments -> ListDeploymentsByTile
handlers/api/v1.listDestinations -> ListOrgs
handlers/api/v1.listDomainResources -> ListDomainResources, ListOrgs
handlers/api/v1.listDomains -> ListDomainsByTile
handlers/api/v1.listInstanceProvisions -> ListProvisionsByInstance
handlers/api/v1.listInvites -> ListInvitesByOrg
handlers/api/v1.listMembers -> ListOrgMembers
handlers/api/v1.listOrgPlans -> ListOrgConfigPlans
handlers/api/v1.listOrgs -> GetOrgMember, ListOrgs, ListOrgsForUser
handlers/api/v1.listPlans -> ListConfigPlans
handlers/api/v1.listRegistries -> ListRegistries
handlers/api/v1.listRegistryCredentials -> ListOrgRegistryCredentials
handlers/api/v1.listRegistryImages -> GetManagedRegistry
handlers/api/v1.listRegistryTags -> GetManagedRegistry
handlers/api/v1.listReleases -> ListConfigPlans, ListDeploymentsByTile
handlers/api/v1.listStacks -> ListOrgs
handlers/api/v1.listStorage -> GetOrg, ListStorage, ListStoragePaths
handlers/api/v1.loadBackup -> GetBackup
handlers/api/v1.loadDeployment -> GetDeployment
handlers/api/v1.newInvite -> CreateInvite
handlers/api/v1.opr -> GetOrg
handlers/api/v1.opw -> GetOrg
handlers/api/v1.orgAllowed -> ListOrgs
handlers/api/v1.orgByPath -> GetOrgBySlug
handlers/api/v1.orgForCreate -> GetOrg, GetOrgMember, ListOrgs
handlers/api/v1.orgShareOwner -> GetOrgBySlug
handlers/api/v1.patchBackup -> GetBackup
handlers/api/v1.patchDestination -> GetBackupDestination, UpdateBackupDestination
handlers/api/v1.patchDomain -> GetDomain
handlers/api/v1.patchDomainResource -> GetOrg, GetOrgBySlug
handlers/api/v1.patchRegistry -> GetRegistry, UpdateRegistry
handlers/api/v1.patchSettingsFor -> GetServer
handlers/api/v1.probeStorage -> GetOrg, GetStorage, ListStoragePaths, UpdateStorage
handlers/api/v1.rejectManagedOwner -> GetOrg
handlers/api/v1.rejectOrgPlan -> GetOrgConfigPlan, SetOrgConfigPlanStatus
handlers/api/v1.rejectPlan -> GetConfigPlan
handlers/api/v1.requireEnvAccess -> GetOrgBySlug
handlers/api/v1.requireOrg -> GetOrg, GetOrgBySlug
handlers/api/v1.requireOrgPlan -> GetOrgConfigPlan
handlers/api/v1.requireOrgRegistry -> GetManagedRegistry
handlers/api/v1.requirePlan -> GetConfigPlan
handlers/api/v1.requireStackAccess -> GetOrg, GetOrgBySlug
handlers/api/v1.requireTile -> GetOrgBySlug, GetTileBySlug
handlers/api/v1.resolvePath -> GetOrgBySlug
handlers/api/v1.resolveResourceTenancy -> GetOrg, GetOrgBySlug
handlers/api/v1.resolveSettingsTarget -> GetServer
handlers/api/v1.restoreBackup -> GetBackup, GetBackupRun
handlers/api/v1.runBackup -> GetBackup
handlers/api/v1.setMemberRole -> GetOrgMember
handlers/api/v1.setProvisionPublic -> GetProvision
handlers/api/v1.settingsFor -> GetServer
handlers/api/v1.setupDone -> ListOrgs
handlers/api/v1.stackByPath -> GetOrgBySlug
handlers/api/v1.storageOut -> GetOrg, ListStoragePaths
handlers/api/v1.tileByPath -> GetOrgBySlug, GetTileBySlug
handlers/api/v1.viewer -> ListOrgs
handlers/web/handler/account.APIKeys -> ListAPIKeys
handlers/web/handler/account.Appearance -> GetUserByID
handlers/web/handler/account.ChangePassword -> GetUserByID
handlers/web/handler/account.DeleteAPIKey -> DeleteAPIKey, ListAPIKeys
handlers/web/handler/account.GraphPrefs -> GetUserByID
handlers/web/handler/account.Notifications -> GetUserByID
handlers/web/handler/account.Profile -> GetUserByID
handlers/web/handler/account.SaveAppearance -> GetUserByID, UpdateUser
handlers/web/handler/account.SaveGraphPrefs -> GetUserByID, UpdateUser
handlers/web/handler/account.SaveNotifications -> GetUserByID, UpdateUser
handlers/web/handler/account.SaveProfile -> GetUserByEmail, GetUserByID, UpdateUser
handlers/web/handler/account.me -> GetUserByID
handlers/web/handler/account.myKeys -> ListAPIKeys
handlers/web/handler/account.ownsAPIKey -> ListAPIKeys
handlers/web/handler/app.Attach -> GetServerByNodeID, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, OpenCronRun, UpdateTile
handlers/web/handler/app.AttachProvision -> GetOrg, GetProvision, ListProvisionsByConsumer
handlers/web/handler/app.AttachStorage -> ListStorage, ListStoragePaths
handlers/web/handler/app.Connectors -> ListConnectorsByOrg
handlers/web/handler/app.CreateAutoDomain -> GetServerByNodeID, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, OpenCronRun
handlers/web/handler/app.CreateDomain -> GetServerByNodeID, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListStagedByEnv, OpenCronRun
handlers/web/handler/app.DeleteDomain -> GetServerByNodeID, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListStagedByEnv, OpenCronRun
handlers/web/handler/app.DeleteVar -> ListAuditEvents, ListStagedByEnv
handlers/web/handler/app.DetachProvision -> ListProvisionsByConsumer
handlers/web/handler/app.DetachStorage -> ListStorage, ListStoragePaths
handlers/web/handler/app.Detail -> GetOrg, GetServerByNodeID, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, OpenCronRun
handlers/web/handler/app.Panel -> GetServerByNodeID, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, OpenCronRun
handlers/web/handler/app.PanelContent -> GetServerByNodeID, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, OpenCronRun
handlers/web/handler/app.PanelHeader -> OpenCronRun
handlers/web/handler/app.Provision -> GetOrg, ListProvisionsByConsumer
handlers/web/handler/app.Provisions -> ListProvisionsByConsumer
handlers/web/handler/app.Restart -> OpenCronRun
handlers/web/handler/app.RunNow -> OpenCronRun
handlers/web/handler/app.RunsLogsStream -> ListCronRuns
handlers/web/handler/app.SaveEnv -> GetServerByNodeID, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, OpenCronRun, UpdateTile
handlers/web/handler/app.SaveSecretVar -> ListAuditEvents, ListStagedByEnv
handlers/web/handler/app.SaveSettings -> GetServerByNodeID, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, OpenCronRun
handlers/web/handler/app.SetDomainCert -> GetServerByNodeID, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, OpenCronRun
handlers/web/handler/app.Stop -> OpenCronRun
handlers/web/handler/app.StopRun -> OpenCronRun
handlers/web/handler/app.StorageFrag -> ListStorage, ListStoragePaths
handlers/web/handler/app.ToggleCron -> OpenCronRun
handlers/web/handler/app.ToggleDomainHTTPS -> GetServerByNodeID, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListStagedByEnv, OpenCronRun
handlers/web/handler/app.Vars -> ListAuditEvents, ListStagedByEnv
handlers/web/handler/app.crumb -> GetOrg
handlers/web/handler/app.currentDesiredDomains -> ListDomainsByTile, ListStagedByEnv
handlers/web/handler/app.currentDesiredEnv -> ListStagedByEnv
handlers/web/handler/app.headerDone -> OpenCronRun
handlers/web/handler/app.infraAddress -> GetOrg
handlers/web/handler/app.loadTab -> GetServerByNodeID, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, OpenCronRun
handlers/web/handler/app.openRun -> OpenCronRun
handlers/web/handler/app.panelDone -> GetServerByNodeID, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, OpenCronRun
handlers/web/handler/app.placementOf -> GetServerByNodeID
handlers/web/handler/app.renderProvisions -> ListProvisionsByConsumer
handlers/web/handler/app.stageDomains -> ListDomainsByTile, ListStagedByEnv
handlers/web/handler/app.storageOptions -> ListStorage, ListStoragePaths
handlers/web/handler/app.wireProvision -> GetOrg
handlers/web/handler/auth/invite.Page -> GetInvite, GetOrg, GetOrgMember, MarkInviteUsed, UpsertOrgMember
handlers/web/handler/auth/invite.Submit -> GetInvite, GetOrg, GetOrgMember, MarkInviteUsed, UpsertOrgMember
handlers/web/handler/auth/invite.join -> GetOrgMember, MarkInviteUsed, UpsertOrgMember
handlers/web/handler/auth/invite.loadInvite -> GetInvite, GetOrg
handlers/web/handler/backups.Create -> GetBackupDestination, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/backups.Delete -> GetBackup, GetBackupDestination, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/backups.History -> GetBackup, GetBackupDestination, ListBackupRuns
handlers/web/handler/backups.Panel -> GetBackupDestination, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/backups.Restore -> GetBackup, GetBackupDestination, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/backups.Run -> GetBackup, GetBackupDestination, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/backups.Save -> GetBackup, GetBackupDestination, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/backups.configView -> GetBackupDestination, ListBackupRuns
handlers/web/handler/backups.load -> GetBackupDestination, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/backups.loadBackup -> GetBackup
handlers/web/handler/backups.render -> GetBackupDestination, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/db.BucketFileDelete -> ListDomainsByTile, ListProvisionsByInstance
handlers/web/handler/db.BucketFileDownload -> ListProvisionsByInstance
handlers/web/handler/db.BucketFileUpload -> ListProvisionsByInstance
handlers/web/handler/db.BucketFiles -> ListProvisionsByInstance
handlers/web/handler/db.BucketPanel -> ListProvisionsByInstance
handlers/web/handler/db.CreateDomain -> ListDomainsByTile
handlers/web/handler/db.Data -> ListProvisionsByInstance
handlers/web/handler/db.DataCell -> ListProvisionsByInstance
handlers/web/handler/db.DataDelete -> ListProvisionsByInstance
handlers/web/handler/db.DataInsert -> ListProvisionsByInstance
handlers/web/handler/db.DataUpdate -> ListProvisionsByInstance
handlers/web/handler/db.Delete -> ListDomainsByTile
handlers/web/handler/db.DeleteDomain -> ListDomainsByTile
handlers/web/handler/db.Detail -> ListDomainsByTile
handlers/web/handler/db.DropProvision -> ListProvisionsByInstance
handlers/web/handler/db.ForkProvision -> GetProvision, ListProvisionsByInstance
handlers/web/handler/db.PGDBPanel -> ListProvisionsByInstance
handlers/web/handler/db.Panel -> ListDomainsByTile
handlers/web/handler/db.PanelContent -> ListDomainsByTile
handlers/web/handler/db.Provisions -> ListProvisionsByInstance
handlers/web/handler/db.SetPort -> ListDomainsByTile
handlers/web/handler/db.SetProvisionPublic -> ListProvisionsByInstance
handlers/web/handler/db.SetScope -> ListDomainsByTile
handlers/web/handler/db.VolumeFileDelete -> ListDomainsByTile
handlers/web/handler/db.loadBucket -> ListProvisionsByInstance
handlers/web/handler/db.loadData -> ListProvisionsByInstance
handlers/web/handler/db.loadTab -> ListDomainsByTile
handlers/web/handler/db.panelDone -> ListDomainsByTile
handlers/web/handler/db.renderProvisions -> ListProvisionsByInstance
handlers/web/handler/deployment.Cancel -> GetDeployment
handlers/web/handler/deployment.Detail -> GetDeployment
handlers/web/handler/deployment.Status -> GetDeployment
handlers/web/handler/deployment.Stream -> GetDeployment
handlers/web/handler/deployment.loadDeployment -> GetDeployment
handlers/web/handler/notification.Badge -> CountUnreadNotifications
handlers/web/handler/notification.Clear -> DeleteAllNotifications
handlers/web/handler/notification.MarkAllRead -> MarkAllNotificationsRead
handlers/web/handler/notification.Page -> ListNotifications, MarkAllNotificationsRead
handlers/web/handler/org.AddMember -> GetOrg, GetOrgBySlug
handlers/web/handler/org.ApproveOrgPlan -> GetOrg, GetOrgBySlug, GetOrgConfigPlan
handlers/web/handler/org.ConnectorBranches -> GetConnector, GetOrg, GetOrgBySlug
handlers/web/handler/org.ConnectorFileExists -> GetConnector, GetOrg, GetOrgBySlug
handlers/web/handler/org.Create -> CreateOrg, GetOrgMember, ListOrgsForUser, UpdateOrg, UpsertOrgMember
handlers/web/handler/org.CreateInvite -> GetOrg, GetOrgBySlug
handlers/web/handler/org.CreateRegistryCredential -> GetManagedRegistry, GetOrg, GetOrgBySlug, GetOrgMember, ListOrgRegistryCredentials
handlers/web/handler/org.Delete -> DeleteOrg, GetOrg, GetOrgBySlug, ListOrgs
handlers/web/handler/org.DeleteAnnotation -> DeleteOrg, GetOrg, GetOrgBySlug, ListOrgs
handlers/web/handler/org.DeleteGraphGroup -> GetOrg, GetOrgBySlug
handlers/web/handler/org.DeleteHomeAnnotation -> DeleteOrg, GetOrg, GetOrgBySlug, ListOrgs
handlers/web/handler/org.DeleteInvite -> DeleteInvite, GetInvite, GetOrg, GetOrgBySlug
handlers/web/handler/org.DeleteOrgDomain -> GetOrg, GetOrgBySlug
handlers/web/handler/org.DeleteOrgVar -> GetOrg, GetOrgBySlug, ListAuditEvents
handlers/web/handler/org.DeleteRegistryCredential -> GetOrg, GetOrgBySlug
handlers/web/handler/org.DeleteRegistryTag -> GetOrg, GetOrgBySlug
handlers/web/handler/org.ExportConfig -> GetOrg, GetOrgBySlug
handlers/web/handler/org.Graph -> CountStacksAwaitingPlan, GetOrg, GetOrgBySlug, GetOrgMember, ListAnnotations, ListConnectorsByOrg, ListDomains, ListGraphGroups, ListNodePositions, ListOrgConfigPlans, ListProvisionsByConsumer
handlers/web/handler/org.GraphStatus -> GetOrg, GetOrgBySlug, ListAnnotations, ListConnectorsByOrg, ListDomains, ListGraphGroups, ListNodePositions, ListProvisionsByConsumer
handlers/web/handler/org.Home -> GetOrgMember, ListAnnotations, ListGraphGroups, ListNodePositions, ListOrgMembers, ListOrgsForUser
handlers/web/handler/org.HomeStatus -> ListAnnotations, ListGraphGroups, ListNodePositions, ListOrgMembers, ListOrgsForUser
handlers/web/handler/org.MoveStack -> GetOrg
handlers/web/handler/org.OrgPlanView -> GetOrg, GetOrgBySlug, GetOrgConfigPlan, LatestWorkItem
handlers/web/handler/org.OrgVarValue -> GetOrg, GetOrgBySlug
handlers/web/handler/org.Plans -> GetOrg, GetOrgBySlug, ListConfigPlans, ListOrgConfigPlans
handlers/web/handler/org.RegistryImages -> GetManagedRegistry, GetOrg, GetOrgBySlug, GetOrgMember, ListDeploymentsByTile
handlers/web/handler/org.ReinviteMember -> DeleteInvite, GetInvite, GetOrg, GetOrgBySlug
handlers/web/handler/org.RejectOrgPlan -> GetOrg, GetOrgBySlug, GetOrgConfigPlan, SetOrgConfigPlanStatus
handlers/web/handler/org.RemoveMember -> GetOrg, GetOrgBySlug
handlers/web/handler/org.Rename -> GetOrg, GetOrgBySlug, ListDomainResources, UpdateOrg
handlers/web/handler/org.ResendInvite -> GetInvite, GetOrg, GetOrgBySlug
handlers/web/handler/org.ResetHomeNodePositions -> DeleteNodePositions
handlers/web/handler/org.ResetNodePositions -> DeleteNodePositions, GetOrg, GetOrgBySlug
handlers/web/handler/org.SaveAnnotation -> GetOrg, GetOrgBySlug
handlers/web/handler/org.SaveDefaults -> GetOrg, GetOrgBySlug
handlers/web/handler/org.SaveEnvColor -> GetOrg, GetOrgBySlug, UpdateOrg
handlers/web/handler/org.SaveGraphGroup -> GetOrg, GetOrgBySlug
handlers/web/handler/org.SaveHomeNodePosition -> SaveNodePositions
handlers/web/handler/org.SaveNodePosition -> GetOrg, GetOrgBySlug, SaveNodePositions
handlers/web/handler/org.SaveOrgConfig -> GetConnector, GetOrg, GetOrgBySlug, UpdateOrg
handlers/web/handler/org.SaveOrgDomain -> GetOrg, GetOrgBySlug
handlers/web/handler/org.SaveOrgVar -> GetOrg, GetOrgBySlug, ListAuditEvents
handlers/web/handler/org.SetMemberRole -> GetOrg, GetOrgBySlug
handlers/web/handler/org.SetPlanInput -> GetOrg, GetOrgBySlug, GetOrgConfigPlan, UpsertVariable
handlers/web/handler/org.SettingsBackups -> GetOrg, GetOrgBySlug
handlers/web/handler/org.SettingsConfig -> GetOrg, GetOrgBySlug, ListConnectorsByOrg, ListOrgConfigPlans
handlers/web/handler/org.SettingsConnectors -> GetOrg, GetOrgBySlug
handlers/web/handler/org.SettingsDefaults -> GetOrg, GetOrgBySlug, GetOrgMember
handlers/web/handler/org.SettingsDomains -> GetOrg, GetOrgBySlug, ListDomainResources
handlers/web/handler/org.SettingsGeneral -> GetOrg, GetOrgBySlug, GetOrgMember
handlers/web/handler/org.SettingsIndex -> GetOrg, GetOrgBySlug
handlers/web/handler/org.SettingsInvites -> GetOrg, GetOrgBySlug
handlers/web/handler/org.SettingsMembers -> GetOrg, GetOrgBySlug, GetOrgMember, ListInvitesByOrg, ListOrgMembers, ListUsers
handlers/web/handler/org.SettingsRegistry -> GetManagedRegistry, GetOrg, GetOrgBySlug, GetOrgMember, ListOrgRegistryCredentials
handlers/web/handler/org.SettingsStorage -> GetOrg, GetOrgBySlug, ListStorage
handlers/web/handler/org.SettingsVariables -> GetOrg, GetOrgBySlug, ListAuditEvents
handlers/web/handler/org.Setup -> CountStacksAwaitingPlan, GetOrg, GetOrgBySlug, ListConnectorsByOrg, ListDomainResources, ListInvitesByOrg, ListOrgConfigPlans, ListOrgMembers, ListUsers
handlers/web/handler/org.SetupApprovePlan -> GetOrg, GetOrgBySlug, GetOrgConfigPlan
handlers/web/handler/org.SetupConfigPlan -> CountStacksAwaitingPlan, GetOrg, GetOrgBySlug, GetOrgConfigPlan, LatestWorkItem, ListOrgConfigPlans
handlers/web/handler/org.SetupDone -> CreateDomainResource, GetOrg, GetOrgBySlug, ListDomainResources, UpdateOrg
handlers/web/handler/org.SetupMode -> CountStacksAwaitingPlan, GetOrg, GetOrgBySlug, ListOrgConfigPlans, SetOrgConfigPlanStatus, UpdateOrg
handlers/web/handler/org.SetupRejectPlan -> GetOrg, GetOrgBySlug, GetOrgConfigPlan, SetOrgConfigPlanStatus
handlers/web/handler/org.VarsPanel -> GetOrg, GetOrgBySlug, ListAuditEvents
handlers/web/handler/org.addCandidates -> ListUsers
handlers/web/handler/org.approvePlan -> GetOrg, GetOrgBySlug, GetOrgConfigPlan
handlers/web/handler/org.buildOrgGraph -> ListAnnotations, ListConnectorsByOrg, ListDomains, ListGraphGroups, ListNodePositions, ListProvisionsByConsumer
handlers/web/handler/org.buildOrgsGraph -> ListAnnotations, ListGraphGroups, ListNodePositions, ListOrgMembers, ListOrgsForUser
handlers/web/handler/org.ensureDefaultDomain -> CreateDomainResource, ListDomainResources
handlers/web/handler/org.githubConnectors -> ListConnectorsByOrg
handlers/web/handler/org.liveTags -> ListDeploymentsByTile
handlers/web/handler/org.loadOrg -> GetOrg, GetOrgBySlug
handlers/web/handler/org.loadOrgPlan -> GetOrg, GetOrgBySlug, GetOrgConfigPlan
handlers/web/handler/org.orgDomains -> ListDomainResources
handlers/web/handler/org.orgPlanWork -> LatestWorkItem
handlers/web/handler/org.ownedInvite -> GetInvite, GetOrg, GetOrgBySlug
handlers/web/handler/org.ownedOrg -> GetOrg, GetOrgBySlug
handlers/web/handler/org.ownedSettingsOrg -> GetOrg, GetOrgBySlug
handlers/web/handler/org.ownerOf -> GetOrgMember
handlers/web/handler/org.pendingOrgPlans -> CountStacksAwaitingPlan, ListOrgConfigPlans
handlers/web/handler/org.pickerConnector -> GetConnector, GetOrg, GetOrgBySlug
handlers/web/handler/org.rejectPlan -> GetOrg, GetOrgBySlug, GetOrgConfigPlan, SetOrgConfigPlanStatus
handlers/web/handler/org.renderRegistry -> GetManagedRegistry, GetOrgMember, ListOrgRegistryCredentials
handlers/web/handler/org.renderVars -> ListAuditEvents
handlers/web/handler/org.settingsOrg -> GetOrg, GetOrgBySlug
handlers/web/handler/org.setupDomainPrefill -> ListDomainResources
handlers/web/handler/org.setupPlan -> CountStacksAwaitingPlan, GetOrgConfigPlan, ListOrgConfigPlans
handlers/web/handler/org.setupSummary -> CountStacksAwaitingPlan, ListConnectorsByOrg, ListDomainResources, ListInvitesByOrg, ListOrgConfigPlans, ListOrgMembers
handlers/web/handler/org.tagRows -> ListDeploymentsByTile
handlers/web/handler/org.unfinishedDraft -> GetOrgMember, ListOrgsForUser
handlers/web/handler/prhook.Hook -> GetConnector, SetSetting
handlers/web/handler/prhook.HookConnector -> GetConnector, GetOrg, SetSetting
handlers/web/handler/prhook.planConfigs -> GetConnector, GetOrg
handlers/web/handler/prhook.updatePlanComment -> GetConnector, SetSetting
handlers/web/handler/project.ApprovePlan -> GetConfigPlan, GetOrg
handlers/web/handler/project.CopyEnv -> GetConnector, GetOrg, ListConfigPlans, ListDeploymentsByTile, ListIntended
handlers/web/handler/project.CreateDB -> GetEnvironment, GetOrg
handlers/web/handler/project.CreateEnvironment -> GetOrg
handlers/web/handler/project.CreateTile -> GetEnvironment, GetOrg
handlers/web/handler/project.Delete -> GetOrg
handlers/web/handler/project.DeleteEnvAnnotation -> GetOrg
handlers/web/handler/project.DeleteEnvVar -> GetOrg, ListAuditEvents
handlers/web/handler/project.DeleteEnvironment -> GetOrg
handlers/web/handler/project.DeleteStackAnnotation -> GetOrg
handlers/web/handler/project.DeleteStackDomain -> GetOrg
handlers/web/handler/project.DeleteStackGraphGroup -> GetOrg
handlers/web/handler/project.DeleteStackVar -> GetOrg, LatestSettledConfigPlan, ListAuditEvents, ListSecretLinks
handlers/web/handler/project.EnvCompare -> GetConnector, GetOrg, ListConfigPlans, ListDeploymentsByTile, ListIntended
handlers/web/handler/project.EnvLogs -> GetOrgBySlug
handlers/web/handler/project.EnvVarValue -> GetOrgBySlug
handlers/web/handler/project.EnvVarsPanel -> GetOrg, GetOrgBySlug, ListAuditEvents
handlers/web/handler/project.ExportConfig -> GetOrgBySlug
handlers/web/handler/project.Graph -> BindingsForConsumer, CountStagedByEnv, GetConnector, GetOrg, GetOrgBySlug, LatestConfigPlan, ListAnnotations, ListConfigPlans, ListDeploymentsByTile, ListDomains, ListDomainsByTile, ListGraphGroups, ListIntended, ListMetrics, ListNodePositions, ListOpenCronRuns, ListProvisionsByConsumer, ListResourcesByEnv, ListStagedByEnv
handlers/web/handler/project.GraphStatus -> BindingsForConsumer, GetOrg, ListAnnotations, ListDomains, ListDomainsByTile, ListGraphGroups, ListMetrics, ListNodePositions, ListOpenCronRuns, ListProvisionsByConsumer, ListResourcesByEnv, ListStagedByEnv
handlers/web/handler/project.MarkIntended -> GetConnector, GetOrg, ListConfigPlans, ListDeploymentsByTile, ListIntended, SetIntended
handlers/web/handler/project.MintStackLink -> GetOrg
handlers/web/handler/project.PlanNow -> GetOrg
handlers/web/handler/project.PlanRedirect -> GetOrg
handlers/web/handler/project.PlanView -> GetConfigPlan, GetOrg, GetOrgBySlug, GetServerByNodeID, LatestWorkItem
handlers/web/handler/project.PromoteCommit -> GetOrg
handlers/web/handler/project.PromoteDialogue -> GetConnector, GetOrg, ListConfigPlans, ListDeploymentsByTile
handlers/web/handler/project.RedirectStack -> GetOrg, GetOrgBySlug
handlers/web/handler/project.RejectPlan -> GetConfigPlan, GetOrg
handlers/web/handler/project.Releases -> GetConnector, GetOrg, GetOrgBySlug, ListConfigPlans, ListDeploymentsByTile
handlers/web/handler/project.Repos -> GetOrg, ListConnectorsByOrg
handlers/web/handler/project.ResetEnvironment -> GetOrg
handlers/web/handler/project.ResetNodePositions -> DeleteNodePositions
handlers/web/handler/project.ResetStackNodePositions -> DeleteNodePositions, GetOrg
handlers/web/handler/project.RevokeStackLink -> GetOrg, ListSecretLinks
handlers/web/handler/project.RotatePRSecret -> GetOrg
handlers/web/handler/project.SaveConfigBinding -> GetOrg
handlers/web/handler/project.SaveEnvColor -> GetOrg
handlers/web/handler/project.SaveEnvConfig -> GetOrg
handlers/web/handler/project.SaveEnvSettings -> GetOrg
handlers/web/handler/project.SaveEnvVar -> GetOrg, ListAuditEvents
handlers/web/handler/project.SaveNodePosition -> SaveNodePositions
handlers/web/handler/project.SavePREnv -> GetOrg
handlers/web/handler/project.SaveSettings -> GetOrg
handlers/web/handler/project.SaveStackAnnotation -> GetOrg
handlers/web/handler/project.SaveStackDomain -> GetOrg
handlers/web/handler/project.SaveStackGraphGroup -> GetOrg
handlers/web/handler/project.SaveStackNodePosition -> GetOrg, SaveNodePositions
handlers/web/handler/project.SaveStackVar -> GetOrg, LatestSettledConfigPlan, ListAuditEvents, ListSecretLinks
handlers/web/handler/project.SetPlanInput -> GetConfigPlan, GetOrg
handlers/web/handler/project.Settings -> GetOrgBySlug
handlers/web/handler/project.SettingsConfig -> GetOrgBySlug, ListConnectorsByOrg
handlers/web/handler/project.SettingsDomains -> GetOrgBySlug, ListDomainResources
handlers/web/handler/project.SettingsEnvironment -> GetOrg, GetOrgBySlug, ListAuditEvents
handlers/web/handler/project.SettingsEnvironments -> GetOrg, GetOrgBySlug
handlers/web/handler/project.SettingsGeneral -> GetOrgBySlug
handlers/web/handler/project.SettingsPREnv -> GetOrgBySlug, ListConnectorsByOrg
handlers/web/handler/project.SettingsVariables -> GetOrg, GetOrgBySlug, LatestSettledConfigPlan, ListAuditEvents, ListSecretLinks
handlers/web/handler/project.StackGraph -> BindingsForConsumer, CountStagedByEnv, GetConnector, GetOrg, GetOrgBySlug, HomeEnvironment, LatestConfigPlan, ListAnnotations, ListConfigPlans, ListDeploymentsByTile, ListDomains, ListDomainsByTile, ListGraphGroups, ListIntended, ListNodePositions, ListProvisionsByConsumer, ListResourcesByEnv
handlers/web/handler/project.StackGraphStatus -> BindingsForConsumer, CountStagedByEnv, GetOrg, HomeEnvironment, ListAnnotations, ListDomains, ListDomainsByTile, ListGraphGroups, ListNodePositions, ListProvisionsByConsumer, ListResourcesByEnv
handlers/web/handler/project.StackVarValue -> GetOrgBySlug
handlers/web/handler/project.StackVarsPanel -> GetOrg, GetOrgBySlug, LatestSettledConfigPlan, ListAuditEvents, ListSecretLinks
handlers/web/handler/project.StagingApply -> GetOrg
handlers/web/handler/project.StagingDiscard -> DeleteStagedByEnv, GetOrg
handlers/web/handler/project.StagingDiscardOne -> CountStagedByEnv, DeleteStagedChange, GetOrg, GetStagedChange
handlers/web/handler/project.StagingReview -> GetOrg, ListStagedByEnv
handlers/web/handler/project.buildGraph -> BindingsForConsumer, GetOrg, ListAnnotations, ListDomains, ListDomainsByTile, ListGraphGroups, ListMetrics, ListNodePositions, ListOpenCronRuns, ListProvisionsByConsumer, ListResourcesByEnv, ListStagedByEnv
handlers/web/handler/project.buildStackGraph -> BindingsForConsumer, CountStagedByEnv, GetOrg, HomeEnvironment, ListAnnotations, ListDomains, ListDomainsByTile, ListGraphGroups, ListNodePositions, ListProvisionsByConsumer, ListResourcesByEnv
handlers/web/handler/project.commitLog -> GetConnector, GetOrg, ListConfigPlans, ListDeploymentsByTile
handlers/web/handler/project.compareEnv -> GetOrg, ListIntended
handlers/web/handler/project.compareStack -> GetOrg, ListIntended
handlers/web/handler/project.envColorsByID -> GetOrg
handlers/web/handler/project.envColorsBySlug -> GetOrg
handlers/web/handler/project.envDeployments -> ListDeploymentsByTile
handlers/web/handler/project.envFromForm -> GetEnvironment
handlers/web/handler/project.envSettingsURL -> GetOrg
handlers/web/handler/project.envVarCards -> GetOrg
handlers/web/handler/project.fetchCommits -> GetConnector
handlers/web/handler/project.fillOrg -> GetOrg
handlers/web/handler/project.githubConnectors -> ListConnectorsByOrg
handlers/web/handler/project.loadCommits -> GetConnector
handlers/web/handler/project.loadPlan -> GetConfigPlan, GetOrg
handlers/web/handler/project.loadStack -> GetOrg
handlers/web/handler/project.loadStagingEnv -> GetOrg
handlers/web/handler/project.olderCommit -> GetConnector
handlers/web/handler/project.releaseView -> GetConnector, GetOrg, ListConfigPlans, ListDeploymentsByTile
handlers/web/handler/project.renderCompare -> GetConnector, GetOrg, ListConfigPlans, ListDeploymentsByTile, ListIntended
handlers/web/handler/project.renderEnvVars -> GetOrg, ListAuditEvents
handlers/web/handler/project.renderStackVars -> GetOrg, LatestSettledConfigPlan, ListAuditEvents, ListSecretLinks
handlers/web/handler/project.resolveSlugs -> GetOrgBySlug
handlers/web/handler/project.resolveStackSlugs -> GetOrgBySlug
handlers/web/handler/project.settingsEnv -> GetOrgBySlug
handlers/web/handler/project.settingsSection -> GetOrg
handlers/web/handler/project.settingsStack -> GetOrgBySlug
handlers/web/handler/project.settingsURL -> GetOrg
handlers/web/handler/project.sharedRefs -> BindingsForConsumer, ListDomainsByTile, ListProvisionsByConsumer, ListResourcesByEnv
handlers/web/handler/project.skippedRungFor -> ListDeploymentsByTile
handlers/web/handler/project.stagedMarkers -> ListStagedByEnv
handlers/web/handler/search.Search -> ListBackups, ListConfigPlans, ListConnectors, ListDomains, ListOrgMembers, ListResourcesByEnv, ListVariableNames
handlers/web/handler/server.Activate -> GetServer
handlers/web/handler/server.CreateDomainResource -> GetServer
handlers/web/handler/server.CreateStoragePath -> GetStorage
handlers/web/handler/server.CreateVolume -> GetServer
handlers/web/handler/server.DeleteStorage -> GetStorage
handlers/web/handler/server.DeleteStoragePath -> DeleteStoragePath, GetStorage, GetStoragePath
handlers/web/handler/server.DeleteVolume -> GetServer
handlers/web/handler/server.Detail -> GetServer, ListDomainResources, ListMetrics, ListStorage, ListStoragePaths
handlers/web/handler/server.Drain -> GetServer
handlers/web/handler/server.DrainForm -> GetServer
handlers/web/handler/server.Host -> GetServer
handlers/web/handler/server.JoinScript -> GetManagedRegistry
handlers/web/handler/server.NewJoinKey -> GetServer
handlers/web/handler/server.ProbeStorage -> GetStorage, UpdateStorage
handlers/web/handler/server.Remove -> GetServer
handlers/web/handler/server.RemoveForm -> GetServer
handlers/web/handler/server.SaveGroup -> GetServer
handlers/web/handler/server.SaveSettings -> GetServer
handlers/web/handler/server.Volumes -> GetServer
handlers/web/handler/server.nodeAction -> GetServer
handlers/web/handler/server.points -> ListMetrics
handlers/web/handler/server.probeAndRecord -> UpdateStorage
handlers/web/handler/server.volumeNode -> GetServer
handlers/web/handler/settings.Audit -> ListAllAuditEvents
handlers/web/handler/settings.Backups -> ListBackupRuns, ListBackups
handlers/web/handler/settings.CreateDestination -> GetOrg, GetOrgBySlug
handlers/web/handler/settings.DeleteConnector -> DeleteConnector, GetConnector, GetOrg
handlers/web/handler/settings.DeleteDestination -> GetBackupDestination, GetOrg, GetOrgBySlug
handlers/web/handler/settings.DeletePanelBackup -> DeleteBackup, ListBackupRuns, ListBackups
handlers/web/handler/settings.GitHubCallback -> GetOrg
handlers/web/handler/settings.GitHubConnect -> GetOrgBySlug
handlers/web/handler/settings.Maintenance -> GetSetting
handlers/web/handler/settings.Registries -> ListRegistries
handlers/web/handler/settings.RunPanelBackup -> ListBackupRuns, ListBackups
handlers/web/handler/settings.SavePanelBackup -> CreateBackup, GetBackupDestination, ListBackupRuns, ListBackups, UpdateBackup
handlers/web/handler/settings.SetRegistryDomain -> GetManagedRegistry
handlers/web/handler/settings.TLS -> GetManagedRegistry, GetSetting
handlers/web/handler/settings.ToggleCleanup -> SetSetting
handlers/web/handler/settings.ToggleDestinationShared -> GetBackupDestination
handlers/web/handler/settings.ToggleUserActive -> GetUserByID, UpdateUser
handlers/web/handler/settings.ToggleUserAdmin -> GetUserByID, ListUsers, UpdateUser
handlers/web/handler/settings.Update -> GetSetting
handlers/web/handler/settings.Users -> ListUsers
handlers/web/handler/settings.destOrg -> GetOrgBySlug
handlers/web/handler/settings.destinationsDone -> GetOrg
handlers/web/handler/settings.panelBackup -> ListBackupRuns, ListBackups
`
