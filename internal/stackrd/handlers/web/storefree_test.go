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
// 417 direct call sites across 79 store methods sit behind these 525 functions.
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
handlers/api/v1.copyEnv -> GetStack
handlers/api/v1.createApp -> ListEnvironmentsByStack
handlers/api/v1.createAutoDomain -> GetEnvironment, ListDomainsByTile
handlers/api/v1.createDB -> GetOrg, ListEnvironmentsByStack
handlers/api/v1.createDomainResource -> GetOrg, GetOrgBySlug, GetStack, GetStackBySlug
handlers/api/v1.createInstanceProvision -> GetTile
handlers/api/v1.createInvite -> CreateInvite
handlers/api/v1.createRegistry -> CreateRegistry
handlers/api/v1.createRegistryCredential -> GetManagedRegistry
handlers/api/v1.createStack -> GetOrg, GetOrgMember, ListOrgs
handlers/api/v1.createStorage -> GetOrg, GetOrgBySlug, ListStoragePaths
handlers/api/v1.createStoragePath -> GetStorage
handlers/api/v1.deleteBackup -> DeleteBackup, GetBackup
handlers/api/v1.deleteDestination -> GetBackupDestination, ListOrgs
handlers/api/v1.deleteDomain -> GetDomain
handlers/api/v1.deleteDomainResource -> GetOrg, GetOrgBySlug, GetStack, GetStackBySlug
handlers/api/v1.deleteEnv -> GetStack
handlers/api/v1.deleteInvite -> DeleteInvite, GetInvite
handlers/api/v1.deleteProvision -> GetProvision
handlers/api/v1.deleteRegistry -> DeleteRegistry, GetRegistry
handlers/api/v1.deleteRegistryTag -> GetManagedRegistry
handlers/api/v1.deleteStorage -> GetOrg, GetOrgBySlug, GetStorage
handlers/api/v1.deleteStoragePath -> DeleteStoragePath, GetStorage, GetStoragePath, ListTiles
handlers/api/v1.deleteVolume -> GetStack
handlers/api/v1.detachProvision -> GetStack
handlers/api/v1.envAndStack -> GetStack
handlers/api/v1.envByPath -> GetEnvironmentBySlug, GetOrgBySlug, GetStackBySlug
handlers/api/v1.envRunning -> ListTilesByEnv
handlers/api/v1.forkProvision -> GetProvision, GetTile
handlers/api/v1.forwardPresence -> GetEnvironment
handlers/api/v1.forwardTile -> GetEnvironment
handlers/api/v1.gate -> GetOrg
handlers/api/v1.getDB -> GetEnvironment, GetOrg, GetStack
handlers/api/v1.getDeployment -> GetDeployment
handlers/api/v1.getDeploymentLogs -> GetDeployment
handlers/api/v1.getEnvVars -> GetEnvironment, ListVariables
handlers/api/v1.getOrgPlan -> GetOrgConfigPlan
handlers/api/v1.getOrgVars -> ListVariables
handlers/api/v1.getPlan -> GetConfigPlan
handlers/api/v1.getRestore -> GetBackup
handlers/api/v1.getStackVars -> ListVariables
handlers/api/v1.getVars -> ListVariables
handlers/api/v1.infraPath -> GetEnvironment, GetOrg, GetStack
handlers/api/v1.listAppProvisions -> ListProvisionsByConsumer
handlers/api/v1.listAppResources -> BindingsForConsumer, GetResource, ListOutputs
handlers/api/v1.listApps -> GetStack, ListOrgs, ListTiles
handlers/api/v1.listBackupRuns -> GetBackup, ListBackupRuns
handlers/api/v1.listBackups -> ListBackupsByTile
handlers/api/v1.listDBs -> GetEnvironment, GetOrg, GetStack, ListOrgs, ListTiles
handlers/api/v1.listDeployments -> ListDeploymentsByTile
handlers/api/v1.listDestinations -> ListOrgs
handlers/api/v1.listDomainResources -> GetStack, ListDomainResources, ListOrgs
handlers/api/v1.listDomains -> ListDomainsByTile
handlers/api/v1.listEnvs -> ListEnvironmentsByStack
handlers/api/v1.listInstanceProvisions -> GetTile, ListProvisionsByInstance
handlers/api/v1.listInvites -> ListInvitesByOrg
handlers/api/v1.listMembers -> ListOrgMembers
handlers/api/v1.listOrgPlans -> ListOrgConfigPlans
handlers/api/v1.listOrgs -> GetOrgMember, ListOrgs, ListOrgsForUser
handlers/api/v1.listPlans -> ListConfigPlans
handlers/api/v1.listRegistries -> ListRegistries
handlers/api/v1.listRegistryCredentials -> ListOrgRegistryCredentials
handlers/api/v1.listRegistryImages -> GetManagedRegistry
handlers/api/v1.listRegistryTags -> GetManagedRegistry
handlers/api/v1.listReleases -> ListConfigPlans, ListDeploymentsByTile, ListEnvironmentsByStack, ListTilesByEnv
handlers/api/v1.listStacks -> ListOrgs, ListStacks
handlers/api/v1.listStorage -> GetOrg, ListStorage, ListStoragePaths
handlers/api/v1.listVolumes -> ListTiles
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
handlers/api/v1.patchDomainResource -> GetOrg, GetOrgBySlug, GetStack, GetStackBySlug
handlers/api/v1.patchEnv -> GetStack
handlers/api/v1.patchRegistry -> GetRegistry, UpdateRegistry
handlers/api/v1.patchSettingsFor -> GetServer
handlers/api/v1.probeStorage -> GetOrg, GetStorage, ListStoragePaths, UpdateStorage
handlers/api/v1.provisionApp -> GetTile
handlers/api/v1.putEnvVars -> GetEnvironment, ListVariables
handlers/api/v1.putOrgVars -> ListVariables
handlers/api/v1.putStackVars -> ListVariables
handlers/api/v1.putVars -> GetStack, ListVariables
handlers/api/v1.rejectManaged -> GetStack
handlers/api/v1.rejectManagedOwner -> GetOrg, GetStack
handlers/api/v1.rejectOrgPlan -> GetOrgConfigPlan, SetOrgConfigPlanStatus
handlers/api/v1.rejectPlan -> GetConfigPlan
handlers/api/v1.requireEnv -> GetEnvironment
handlers/api/v1.requireEnvAccess -> GetEnvironment, GetEnvironmentBySlug, GetOrgBySlug, GetStackBySlug
handlers/api/v1.requireOrg -> GetOrg, GetOrgBySlug
handlers/api/v1.requireOrgPlan -> GetOrgConfigPlan
handlers/api/v1.requireOrgRegistry -> GetManagedRegistry
handlers/api/v1.requirePlan -> GetConfigPlan
handlers/api/v1.requireStackAccess -> GetOrg, GetOrgBySlug, GetStack, GetStackBySlug
handlers/api/v1.requireTile -> GetEnvironmentBySlug, GetOrgBySlug, GetStackBySlug, GetTile, GetTileBySlug
handlers/api/v1.resetEnv -> GetStack
handlers/api/v1.resolveEnv -> ListEnvironmentsByStack
handlers/api/v1.resolvePath -> GetEnvironment, GetEnvironmentBySlug, GetOrgBySlug, GetStackBySlug
handlers/api/v1.resolveResourceTenancy -> GetOrg, GetOrgBySlug, GetStack, GetStackBySlug
handlers/api/v1.resolveSettingsTarget -> GetServer
handlers/api/v1.restoreBackup -> GetBackup, GetBackupRun
handlers/api/v1.runBackup -> GetBackup
handlers/api/v1.setMemberRole -> GetOrgMember
handlers/api/v1.setProvisionPublic -> GetProvision, GetTile
handlers/api/v1.settingsFor -> GetServer
handlers/api/v1.setupDone -> ListOrgs
handlers/api/v1.stackByPath -> GetOrgBySlug, GetStackBySlug
handlers/api/v1.stackOrg -> GetStack
handlers/api/v1.storageOut -> GetOrg, ListStoragePaths
handlers/api/v1.tileByPath -> GetEnvironmentBySlug, GetOrgBySlug, GetStackBySlug, GetTileBySlug
handlers/api/v1.toSliceOut -> GetTile
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
handlers/web/handler/app.Attach -> GetServerByNodeID, GetStack, GetTile, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListTilesByEnv, OpenCronRun, UpdateTile
handlers/web/handler/app.AttachProvision -> GetOrg, GetProvision, GetStack, GetTile, ListProvisionsByConsumer
handlers/web/handler/app.AttachStorage -> GetTile, ListStorage, ListStoragePaths
handlers/web/handler/app.Branches -> GetTile
handlers/web/handler/app.Connectors -> GetStack, GetTile, ListConnectorsByOrg
handlers/web/handler/app.CreateAutoDomain -> GetEnvironment, GetServerByNodeID, GetStack, GetTile, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListTilesByEnv, OpenCronRun
handlers/web/handler/app.CreateDomain -> GetServerByNodeID, GetStack, GetTile, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListStagedByEnv, ListTilesByEnv, OpenCronRun
handlers/web/handler/app.Delete -> GetTile
handlers/web/handler/app.DeleteDomain -> GetServerByNodeID, GetStack, GetTile, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListStagedByEnv, ListTilesByEnv, OpenCronRun
handlers/web/handler/app.DeleteVar -> GetStack, GetTile, ListAuditEvents, ListStagedByEnv, ListVariables
handlers/web/handler/app.DetachProvision -> GetStack, GetTile, ListProvisionsByConsumer
handlers/web/handler/app.DetachStorage -> GetTile, ListStorage, ListStoragePaths
handlers/web/handler/app.Detail -> GetEnvironment, GetOrg, GetServerByNodeID, GetStack, GetTile, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListTilesByEnv, OpenCronRun
handlers/web/handler/app.LogsStream -> GetTile
handlers/web/handler/app.Metrics -> GetTile
handlers/web/handler/app.Panel -> GetServerByNodeID, GetStack, GetTile, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListTilesByEnv, OpenCronRun
handlers/web/handler/app.PanelContent -> GetServerByNodeID, GetStack, GetTile, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListTilesByEnv, OpenCronRun
handlers/web/handler/app.PanelHeader -> GetTile, OpenCronRun
handlers/web/handler/app.Provision -> GetOrg, GetStack, GetTile, ListProvisionsByConsumer
handlers/web/handler/app.Provisions -> GetTile, ListProvisionsByConsumer
handlers/web/handler/app.Repos -> GetTile
handlers/web/handler/app.Restart -> GetTile, OpenCronRun
handlers/web/handler/app.RunNow -> GetTile, OpenCronRun
handlers/web/handler/app.RunsLogsStream -> GetTile, ListCronRuns
handlers/web/handler/app.SaveEnv -> GetServerByNodeID, GetStack, GetTile, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListTilesByEnv, OpenCronRun, UpdateTile
handlers/web/handler/app.SaveSecretVar -> GetStack, GetTile, ListAuditEvents, ListStagedByEnv, ListVariables
handlers/web/handler/app.SaveSettings -> GetServerByNodeID, GetStack, GetTile, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListTilesByEnv, OpenCronRun
handlers/web/handler/app.SetDomainCert -> GetServerByNodeID, GetStack, GetTile, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListTilesByEnv, OpenCronRun
handlers/web/handler/app.Size -> GetTile
handlers/web/handler/app.Stop -> GetTile, OpenCronRun
handlers/web/handler/app.StopRun -> GetTile, OpenCronRun
handlers/web/handler/app.StorageFrag -> GetTile, ListStorage, ListStoragePaths
handlers/web/handler/app.ToggleCron -> GetTile, OpenCronRun
handlers/web/handler/app.ToggleDomainHTTPS -> GetServerByNodeID, GetStack, GetTile, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListStagedByEnv, ListTilesByEnv, OpenCronRun
handlers/web/handler/app.VarValue -> GetStack, GetTile, ListVariables
handlers/web/handler/app.Vars -> GetStack, GetTile, ListAuditEvents, ListStagedByEnv, ListVariables
handlers/web/handler/app.VolumeFileDelete -> GetTile
handlers/web/handler/app.VolumeFileDownload -> GetTile
handlers/web/handler/app.VolumeFileUpload -> GetTile
handlers/web/handler/app.VolumeFiles -> GetTile
handlers/web/handler/app.configMode -> GetStack
handlers/web/handler/app.crumb -> GetEnvironment, GetOrg, GetStack
handlers/web/handler/app.currentDesiredDomains -> ListDomainsByTile, ListStagedByEnv
handlers/web/handler/app.currentDesiredEnv -> ListStagedByEnv
handlers/web/handler/app.envServices -> ListTilesByEnv
handlers/web/handler/app.headerDone -> OpenCronRun
handlers/web/handler/app.infraAddress -> GetOrg, GetStack
handlers/web/handler/app.load -> GetTile
handlers/web/handler/app.loadTab -> GetServerByNodeID, GetStack, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListTilesByEnv, OpenCronRun
handlers/web/handler/app.loadVolumeTile -> GetTile
handlers/web/handler/app.openRun -> OpenCronRun
handlers/web/handler/app.panelDone -> GetServerByNodeID, GetStack, ListCronRuns, ListDeploymentsByTile, ListDomainsByTile, ListTilesByEnv, OpenCronRun
handlers/web/handler/app.placementOf -> GetServerByNodeID
handlers/web/handler/app.rejectManaged -> GetStack
handlers/web/handler/app.renderProvisions -> GetTile, ListProvisionsByConsumer
handlers/web/handler/app.stageDomains -> ListDomainsByTile, ListStagedByEnv
handlers/web/handler/app.storageOptions -> ListStorage, ListStoragePaths
handlers/web/handler/app.uiManaged -> GetStack
handlers/web/handler/app.volumeView -> ListTilesByEnv
handlers/web/handler/app.wireProvision -> GetOrg, GetStack
handlers/web/handler/auth/invite.Page -> GetInvite, GetOrg, GetOrgMember, MarkInviteUsed, UpsertOrgMember
handlers/web/handler/auth/invite.Submit -> GetInvite, GetOrg, GetOrgMember, MarkInviteUsed, UpsertOrgMember
handlers/web/handler/auth/invite.join -> GetOrgMember, MarkInviteUsed, UpsertOrgMember
handlers/web/handler/auth/invite.loadInvite -> GetInvite, GetOrg
handlers/web/handler/backups.Create -> GetBackupDestination, GetTile, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/backups.Delete -> GetBackup, GetBackupDestination, GetTile, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/backups.History -> GetBackup, GetBackupDestination, GetTile, ListBackupRuns
handlers/web/handler/backups.Panel -> GetBackupDestination, GetTile, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/backups.Restore -> GetBackup, GetBackupDestination, GetTile, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/backups.Run -> GetBackup, GetBackupDestination, GetTile, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/backups.Save -> GetBackup, GetBackupDestination, GetTile, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/backups.configView -> GetBackupDestination, ListBackupRuns
handlers/web/handler/backups.load -> GetBackupDestination, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/backups.loadBackup -> GetBackup, GetTile
handlers/web/handler/backups.loadTile -> GetTile
handlers/web/handler/backups.render -> GetBackupDestination, ListBackupRuns, ListBackupsByTile, ListProvisionsByInstance
handlers/web/handler/db.BucketFileDelete -> GetStack, GetTile, ListDomainsByTile, ListProvisionsByInstance
handlers/web/handler/db.BucketFileDownload -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.BucketFileUpload -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.BucketFiles -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.BucketPanel -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.CreateDomain -> GetStack, GetTile, ListDomainsByTile
handlers/web/handler/db.Data -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.DataCell -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.DataDelete -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.DataInsert -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.DataUpdate -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.Delete -> GetStack, GetTile, ListDomainsByTile
handlers/web/handler/db.DeleteDomain -> GetStack, GetTile, ListDomainsByTile
handlers/web/handler/db.Deploy -> GetTile
handlers/web/handler/db.Detail -> GetStack, GetTile, ListDomainsByTile
handlers/web/handler/db.DropProvision -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.ForkProvision -> GetProvision, GetTile, ListProvisionsByInstance
handlers/web/handler/db.LogsStream -> GetTile
handlers/web/handler/db.Metrics -> GetTile
handlers/web/handler/db.PGDBPanel -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.Panel -> GetStack, GetTile, ListDomainsByTile
handlers/web/handler/db.PanelContent -> GetStack, GetTile, ListDomainsByTile
handlers/web/handler/db.Provisions -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.SetPort -> GetStack, GetTile, ListDomainsByTile
handlers/web/handler/db.SetProvisionPublic -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.SetScope -> GetStack, GetTile, ListDomainsByTile
handlers/web/handler/db.Start -> GetTile
handlers/web/handler/db.Stop -> GetTile
handlers/web/handler/db.VolumeFileDelete -> GetStack, GetTile, ListDomainsByTile
handlers/web/handler/db.VolumeFileDownload -> GetTile
handlers/web/handler/db.VolumeFileUpload -> GetTile
handlers/web/handler/db.VolumeFiles -> GetTile
handlers/web/handler/db.VolumePanel -> GetTile
handlers/web/handler/db.load -> GetTile
handlers/web/handler/db.loadBucket -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.loadData -> GetTile, ListProvisionsByInstance
handlers/web/handler/db.loadTab -> GetStack, ListDomainsByTile
handlers/web/handler/db.loadVolume -> GetTile
handlers/web/handler/db.panelDone -> GetStack, ListDomainsByTile
handlers/web/handler/db.renderProvisions -> GetTile, ListProvisionsByInstance
handlers/web/handler/deployment.Cancel -> GetDeployment, GetTile
handlers/web/handler/deployment.Deploy -> GetTile
handlers/web/handler/deployment.Detail -> GetDeployment, GetTile
handlers/web/handler/deployment.Rollback -> GetTile
handlers/web/handler/deployment.Status -> GetDeployment, GetTile
handlers/web/handler/deployment.Stream -> GetDeployment, GetTile
handlers/web/handler/deployment.loadDeployment -> GetDeployment, GetTile
handlers/web/handler/deployment.loadTile -> GetTile
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
handlers/web/handler/org.Delete -> DeleteOrg, GetOrg, GetOrgBySlug, ListOrgs, ListStacksByOrg
handlers/web/handler/org.DeleteAnnotation -> DeleteOrg, GetOrg, GetOrgBySlug, ListOrgs, ListStacksByOrg
handlers/web/handler/org.DeleteGraphGroup -> GetOrg, GetOrgBySlug
handlers/web/handler/org.DeleteHomeAnnotation -> DeleteOrg, GetOrg, GetOrgBySlug, ListOrgs, ListStacksByOrg
handlers/web/handler/org.DeleteInvite -> DeleteInvite, GetInvite, GetOrg, GetOrgBySlug
handlers/web/handler/org.DeleteOrgDomain -> GetOrg, GetOrgBySlug
handlers/web/handler/org.DeleteOrgVar -> GetOrg, GetOrgBySlug, ListAuditEvents, ListVariables
handlers/web/handler/org.DeleteRegistryCredential -> GetOrg, GetOrgBySlug
handlers/web/handler/org.DeleteRegistryTag -> GetOrg, GetOrgBySlug
handlers/web/handler/org.ExportConfig -> GetOrg, GetOrgBySlug
handlers/web/handler/org.Graph -> CountStacksAwaitingPlan, GetOrg, GetOrgBySlug, GetOrgMember, GetTile, ListAnnotations, ListConnectorsByOrg, ListDomains, ListEnvironmentsByStack, ListGraphGroups, ListNodePositions, ListOrgConfigPlans, ListProvisionsByConsumer, ListStacksByOrg, ListTilesByStack, ListVariables
handlers/web/handler/org.GraphStatus -> GetOrg, GetOrgBySlug, GetTile, ListAnnotations, ListConnectorsByOrg, ListDomains, ListEnvironmentsByStack, ListGraphGroups, ListNodePositions, ListProvisionsByConsumer, ListStacksByOrg, ListTilesByStack, ListVariables
handlers/web/handler/org.Home -> GetOrgMember, ListAnnotations, ListGraphGroups, ListNodePositions, ListOrgMembers, ListOrgsForUser, ListStacksByOrg
handlers/web/handler/org.HomeStatus -> ListAnnotations, ListGraphGroups, ListNodePositions, ListOrgMembers, ListOrgsForUser, ListStacksByOrg
handlers/web/handler/org.MoveStack -> GetOrg, GetStack
handlers/web/handler/org.OrgPlanView -> GetOrg, GetOrgBySlug, GetOrgConfigPlan, LatestWorkItem
handlers/web/handler/org.OrgVarValue -> GetOrg, GetOrgBySlug, ListVariables
handlers/web/handler/org.Plans -> GetOrg, GetOrgBySlug, ListConfigPlans, ListOrgConfigPlans, ListStacksByOrg
handlers/web/handler/org.RegistryImages -> GetManagedRegistry, GetOrg, GetOrgBySlug, GetOrgMember, ListDeploymentsByTile, ListStacksByOrg, ListTilesByStack
handlers/web/handler/org.ReinviteMember -> DeleteInvite, GetInvite, GetOrg, GetOrgBySlug
handlers/web/handler/org.RejectOrgPlan -> GetOrg, GetOrgBySlug, GetOrgConfigPlan, SetOrgConfigPlanStatus
handlers/web/handler/org.RemoveMember -> GetOrg, GetOrgBySlug
handlers/web/handler/org.Rename -> GetOrg, GetOrgBySlug, ListDomainResources, ListStacksByOrg, UpdateOrg
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
handlers/web/handler/org.SaveOrgVar -> GetOrg, GetOrgBySlug, ListAuditEvents, ListVariables
handlers/web/handler/org.SetMemberRole -> GetOrg, GetOrgBySlug
handlers/web/handler/org.SetPlanInput -> GetOrg, GetOrgBySlug, GetOrgConfigPlan, UpsertVariable
handlers/web/handler/org.SettingsBackups -> GetOrg, GetOrgBySlug
handlers/web/handler/org.SettingsConfig -> GetOrg, GetOrgBySlug, ListConnectorsByOrg, ListOrgConfigPlans
handlers/web/handler/org.SettingsConnectors -> GetOrg, GetOrgBySlug
handlers/web/handler/org.SettingsDefaults -> GetOrg, GetOrgBySlug, GetOrgMember
handlers/web/handler/org.SettingsDomains -> GetOrg, GetOrgBySlug, ListDomainResources
handlers/web/handler/org.SettingsGeneral -> GetOrg, GetOrgBySlug, GetOrgMember, ListEnvironmentsByStack, ListStacksByOrg
handlers/web/handler/org.SettingsIndex -> GetOrg, GetOrgBySlug
handlers/web/handler/org.SettingsInvites -> GetOrg, GetOrgBySlug
handlers/web/handler/org.SettingsMembers -> GetOrg, GetOrgBySlug, GetOrgMember, ListInvitesByOrg, ListOrgMembers, ListUsers
handlers/web/handler/org.SettingsRegistry -> GetManagedRegistry, GetOrg, GetOrgBySlug, GetOrgMember, ListOrgRegistryCredentials
handlers/web/handler/org.SettingsStorage -> GetOrg, GetOrgBySlug, ListStorage
handlers/web/handler/org.SettingsVariables -> GetOrg, GetOrgBySlug, ListAuditEvents, ListVariables
handlers/web/handler/org.Setup -> CountStacksAwaitingPlan, GetOrg, GetOrgBySlug, ListConnectorsByOrg, ListDomainResources, ListInvitesByOrg, ListOrgConfigPlans, ListOrgMembers, ListUsers
handlers/web/handler/org.SetupApprovePlan -> GetOrg, GetOrgBySlug, GetOrgConfigPlan
handlers/web/handler/org.SetupConfigPlan -> CountStacksAwaitingPlan, GetOrg, GetOrgBySlug, GetOrgConfigPlan, LatestWorkItem, ListOrgConfigPlans
handlers/web/handler/org.SetupDone -> CreateDomainResource, GetOrg, GetOrgBySlug, ListDomainResources, UpdateOrg
handlers/web/handler/org.SetupMode -> CountStacksAwaitingPlan, GetOrg, GetOrgBySlug, ListOrgConfigPlans, SetOrgConfigPlanStatus, UpdateOrg
handlers/web/handler/org.SetupRejectPlan -> GetOrg, GetOrgBySlug, GetOrgConfigPlan, SetOrgConfigPlanStatus
handlers/web/handler/org.VarsPanel -> GetOrg, GetOrgBySlug, ListAuditEvents, ListVariables
handlers/web/handler/org.addCandidates -> ListUsers
handlers/web/handler/org.approvePlan -> GetOrg, GetOrgBySlug, GetOrgConfigPlan
handlers/web/handler/org.buildOrgGraph -> GetTile, ListAnnotations, ListConnectorsByOrg, ListDomains, ListEnvironmentsByStack, ListGraphGroups, ListNodePositions, ListProvisionsByConsumer, ListStacksByOrg, ListTilesByStack, ListVariables
handlers/web/handler/org.buildOrgsGraph -> ListAnnotations, ListGraphGroups, ListNodePositions, ListOrgMembers, ListOrgsForUser, ListStacksByOrg
handlers/web/handler/org.ensureDefaultDomain -> CreateDomainResource, ListDomainResources
handlers/web/handler/org.envColorRows -> ListEnvironmentsByStack
handlers/web/handler/org.githubConnectors -> ListConnectorsByOrg
handlers/web/handler/org.liveTags -> ListDeploymentsByTile, ListStacksByOrg, ListTilesByStack
handlers/web/handler/org.loadOrg -> GetOrg, GetOrgBySlug
handlers/web/handler/org.loadOrgPlan -> GetOrg, GetOrgBySlug, GetOrgConfigPlan
handlers/web/handler/org.orgDomains -> ListDomainResources
handlers/web/handler/org.orgPlanWork -> LatestWorkItem
handlers/web/handler/org.orgVarCards -> ListTilesByStack, ListVariables
handlers/web/handler/org.ownedInvite -> GetInvite, GetOrg, GetOrgBySlug
handlers/web/handler/org.ownedOrg -> GetOrg, GetOrgBySlug
handlers/web/handler/org.ownedSettingsOrg -> GetOrg, GetOrgBySlug
handlers/web/handler/org.ownerOf -> GetOrgMember
handlers/web/handler/org.pendingOrgPlans -> CountStacksAwaitingPlan, ListOrgConfigPlans
handlers/web/handler/org.pickerConnector -> GetConnector, GetOrg, GetOrgBySlug
handlers/web/handler/org.rejectPlan -> GetOrg, GetOrgBySlug, GetOrgConfigPlan, SetOrgConfigPlanStatus
handlers/web/handler/org.renderRegistry -> GetManagedRegistry, GetOrgMember, ListOrgRegistryCredentials
handlers/web/handler/org.renderVars -> ListAuditEvents, ListVariables
handlers/web/handler/org.settingsOrg -> GetOrg, GetOrgBySlug
handlers/web/handler/org.setupDomainPrefill -> ListDomainResources
handlers/web/handler/org.setupPlan -> CountStacksAwaitingPlan, GetOrgConfigPlan, ListOrgConfigPlans
handlers/web/handler/org.setupSummary -> CountStacksAwaitingPlan, ListConnectorsByOrg, ListDomainResources, ListInvitesByOrg, ListOrgConfigPlans, ListOrgMembers
handlers/web/handler/org.tagRows -> ListDeploymentsByTile, ListStacksByOrg, ListTilesByStack
handlers/web/handler/org.unfinishedDraft -> GetOrgMember, ListOrgsForUser
handlers/web/handler/prhook.Hook -> GetConnector, GetEnvironmentBySlug, GetStack, ListEnvironmentsByStack, ListTilesByEnv, SetSetting
handlers/web/handler/prhook.HookConnector -> GetConnector, GetEnvironmentBySlug, GetOrg, ListEnvironmentsByStack, ListStacks, ListTilesByEnv, SetSetting
handlers/web/handler/prhook.autoDeploy -> ListEnvironmentsByStack, ListStacks, ListTilesByEnv
handlers/web/handler/prhook.closePR -> GetEnvironmentBySlug
handlers/web/handler/prhook.dispatch -> GetEnvironmentBySlug, ListEnvironmentsByStack, ListTilesByEnv
handlers/web/handler/prhook.openPR -> GetEnvironmentBySlug, ListEnvironmentsByStack, ListTilesByEnv
handlers/web/handler/prhook.planConfigs -> GetConnector, GetOrg, ListEnvironmentsByStack, ListStacks
handlers/web/handler/prhook.stackTracksRepo -> ListEnvironmentsByStack, ListTilesByEnv
handlers/web/handler/prhook.syncPR -> GetEnvironmentBySlug, ListTilesByEnv
handlers/web/handler/prhook.updatePlanComment -> GetConnector, GetEnvironmentBySlug, SetSetting
handlers/web/handler/project.ApprovePlan -> GetConfigPlan, GetOrg, GetStack
handlers/web/handler/project.CopyEnv -> GetConnector, GetEnvironment, GetOrg, GetStack, ListConfigPlans, ListDeploymentsByTile, ListEnvironmentsByStack, ListIntended, ListTilesByEnv
handlers/web/handler/project.CreateDB -> GetEnvironment, GetOrg, GetStack, ListEnvironmentsByStack
handlers/web/handler/project.CreateEnvironment -> GetOrg, GetStack
handlers/web/handler/project.CreateTile -> GetEnvironment, GetOrg, GetStack, GetTile, ListEnvironmentsByStack
handlers/web/handler/project.Delete -> GetOrg, GetStack
handlers/web/handler/project.DeleteEnvAnnotation -> GetEnvironment, GetOrg, GetStack
handlers/web/handler/project.DeleteEnvGraphGroup -> GetEnvironment
handlers/web/handler/project.DeleteEnvVar -> GetEnvironment, GetOrg, GetStack, ListAuditEvents, ListEnvironmentsByStack, ListTilesByEnv, ListVariables
handlers/web/handler/project.DeleteEnvironment -> GetEnvironment, GetOrg, GetStack
handlers/web/handler/project.DeleteStackAnnotation -> GetOrg, GetStack
handlers/web/handler/project.DeleteStackDomain -> GetOrg, GetStack
handlers/web/handler/project.DeleteStackGraphGroup -> GetOrg, GetStack
handlers/web/handler/project.DeleteStackVar -> GetOrg, GetStack, LatestSettledConfigPlan, ListAuditEvents, ListEnvironmentsByStack, ListSecretLinks, ListVariables
handlers/web/handler/project.EnvCompare -> GetConnector, GetOrg, GetStack, ListConfigPlans, ListDeploymentsByTile, ListEnvironmentsByStack, ListIntended, ListTilesByEnv
handlers/web/handler/project.EnvLogs -> GetEnvironmentBySlug, GetOrgBySlug, GetStackBySlug
handlers/web/handler/project.EnvLogsStream -> GetEnvironment, ListTilesByEnv
handlers/web/handler/project.EnvVarValue -> GetEnvironmentBySlug, GetOrgBySlug, GetStackBySlug, ListVariables
handlers/web/handler/project.EnvVarsPanel -> GetEnvironmentBySlug, GetOrg, GetOrgBySlug, GetStackBySlug, ListAuditEvents, ListEnvironmentsByStack, ListTilesByEnv, ListVariables
handlers/web/handler/project.ExportConfig -> GetOrgBySlug, GetStackBySlug
handlers/web/handler/project.Graph -> BindingsForConsumer, CountStagedByEnv, GetConnector, GetEnvironment, GetEnvironmentBySlug, GetOrg, GetOrgBySlug, GetStack, GetStackBySlug, GetTile, LatestConfigPlan, ListAnnotations, ListConfigPlans, ListDeploymentsByTile, ListDomains, ListDomainsByTile, ListEnvironmentsByStack, ListGraphGroups, ListIntended, ListMetrics, ListNodePositions, ListOpenCronRuns, ListProvisionsByConsumer, ListResourcesByEnv, ListStagedByEnv, ListTilesByEnv, ListVariables
handlers/web/handler/project.GraphStatus -> BindingsForConsumer, GetEnvironment, GetOrg, GetStack, GetTile, ListAnnotations, ListDomains, ListDomainsByTile, ListGraphGroups, ListMetrics, ListNodePositions, ListOpenCronRuns, ListProvisionsByConsumer, ListResourcesByEnv, ListStagedByEnv, ListTilesByEnv, ListVariables
handlers/web/handler/project.MarkIntended -> GetConnector, GetEnvironment, GetOrg, GetStack, ListConfigPlans, ListDeploymentsByTile, ListEnvironmentsByStack, ListIntended, ListTilesByEnv, SetIntended
handlers/web/handler/project.MintStackLink -> GetOrg, GetStack
handlers/web/handler/project.PlanNow -> GetOrg, GetStack
handlers/web/handler/project.PlanRedirect -> GetOrg, GetStack
handlers/web/handler/project.PlanView -> GetConfigPlan, GetOrg, GetOrgBySlug, GetServerByNodeID, GetStackBySlug, GetTile, LatestWorkItem, ListEnvironmentsByStack
handlers/web/handler/project.PromoteCommit -> GetOrg, GetStack
handlers/web/handler/project.PromoteDialogue -> GetConnector, GetOrg, GetStack, ListConfigPlans, ListDeploymentsByTile, ListEnvironmentsByStack, ListTilesByEnv
handlers/web/handler/project.RedirectStack -> GetOrg, GetOrgBySlug, GetStack, GetStackBySlug
handlers/web/handler/project.RejectPlan -> GetConfigPlan, GetOrg, GetStack
handlers/web/handler/project.Releases -> GetConnector, GetOrg, GetOrgBySlug, GetStackBySlug, ListConfigPlans, ListDeploymentsByTile, ListEnvironmentsByStack, ListTilesByEnv
handlers/web/handler/project.Repos -> GetOrg, GetStack, ListConnectorsByOrg
handlers/web/handler/project.ResetEnvironment -> GetEnvironment, GetOrg, GetStack
handlers/web/handler/project.ResetNodePositions -> DeleteNodePositions, GetEnvironment
handlers/web/handler/project.ResetStackNodePositions -> DeleteNodePositions, GetOrg, GetStack
handlers/web/handler/project.RevokeStackLink -> GetOrg, GetStack, ListSecretLinks
handlers/web/handler/project.RotatePRSecret -> GetOrg, GetStack
handlers/web/handler/project.SaveConfigBinding -> GetOrg, GetStack
handlers/web/handler/project.SaveEnvAnnotation -> GetEnvironment
handlers/web/handler/project.SaveEnvColor -> GetEnvironment, GetOrg, GetStack
handlers/web/handler/project.SaveEnvConfig -> GetEnvironment, GetOrg, GetStack
handlers/web/handler/project.SaveEnvGraphGroup -> GetEnvironment
handlers/web/handler/project.SaveEnvSettings -> GetEnvironment, GetOrg, GetStack
handlers/web/handler/project.SaveEnvVar -> GetEnvironment, GetOrg, GetStack, ListAuditEvents, ListEnvironmentsByStack, ListTilesByEnv, ListVariables
handlers/web/handler/project.SaveNodePosition -> GetEnvironment, ListTilesByEnv, SaveNodePositions
handlers/web/handler/project.SavePREnv -> GetOrg, GetStack
handlers/web/handler/project.SaveSettings -> GetOrg, GetStack
handlers/web/handler/project.SaveStackAnnotation -> GetOrg, GetStack
handlers/web/handler/project.SaveStackDomain -> GetOrg, GetStack
handlers/web/handler/project.SaveStackGraphGroup -> GetOrg, GetStack
handlers/web/handler/project.SaveStackNodePosition -> GetOrg, GetStack, SaveNodePositions
handlers/web/handler/project.SaveStackVar -> GetOrg, GetStack, LatestSettledConfigPlan, ListAuditEvents, ListEnvironmentsByStack, ListSecretLinks, ListVariables
handlers/web/handler/project.SetPlanInput -> GetConfigPlan, GetOrg, GetStack
handlers/web/handler/project.Settings -> GetOrgBySlug, GetStackBySlug
handlers/web/handler/project.SettingsConfig -> GetOrgBySlug, GetStackBySlug, ListConnectorsByOrg
handlers/web/handler/project.SettingsDomains -> GetOrgBySlug, GetStackBySlug, ListDomainResources
handlers/web/handler/project.SettingsEnvironment -> GetOrg, GetOrgBySlug, GetStackBySlug, ListAuditEvents, ListEnvironmentsByStack, ListTilesByEnv, ListVariables
handlers/web/handler/project.SettingsEnvironments -> GetOrg, GetOrgBySlug, GetStackBySlug, ListEnvironmentsByStack, ListTilesByEnv
handlers/web/handler/project.SettingsGeneral -> GetOrgBySlug, GetStackBySlug
handlers/web/handler/project.SettingsPREnv -> GetOrgBySlug, GetStackBySlug, ListConnectorsByOrg, ListEnvironmentsByStack
handlers/web/handler/project.SettingsVariables -> GetOrg, GetOrgBySlug, GetStackBySlug, LatestSettledConfigPlan, ListAuditEvents, ListEnvironmentsByStack, ListSecretLinks, ListVariables
handlers/web/handler/project.StackGraph -> BindingsForConsumer, CountStagedByEnv, GetConnector, GetOrg, GetOrgBySlug, GetStackBySlug, GetTile, HomeEnvironment, LatestConfigPlan, ListAnnotations, ListConfigPlans, ListDeploymentsByTile, ListDomains, ListDomainsByTile, ListEnvironmentsByStack, ListGraphGroups, ListIntended, ListNodePositions, ListProvisionsByConsumer, ListResourcesByEnv, ListTilesByEnv, ListVariables
handlers/web/handler/project.StackGraphStatus -> BindingsForConsumer, CountStagedByEnv, GetOrg, GetStack, GetTile, HomeEnvironment, ListAnnotations, ListDomains, ListDomainsByTile, ListEnvironmentsByStack, ListGraphGroups, ListNodePositions, ListProvisionsByConsumer, ListResourcesByEnv, ListTilesByEnv, ListVariables
handlers/web/handler/project.StackVarValue -> GetOrgBySlug, GetStackBySlug, ListVariables
handlers/web/handler/project.StackVarsPanel -> GetOrg, GetOrgBySlug, GetStackBySlug, LatestSettledConfigPlan, ListAuditEvents, ListEnvironmentsByStack, ListSecretLinks, ListVariables
handlers/web/handler/project.StagingApply -> GetEnvironment, GetOrg, GetStack
handlers/web/handler/project.StagingDiscard -> DeleteStagedByEnv, GetEnvironment, GetOrg, GetStack
handlers/web/handler/project.StagingDiscardOne -> CountStagedByEnv, DeleteStagedChange, GetEnvironment, GetOrg, GetStack, GetStagedChange
handlers/web/handler/project.StagingReview -> GetEnvironment, GetOrg, GetStack, ListStagedByEnv
handlers/web/handler/project.buildGraph -> BindingsForConsumer, GetEnvironment, GetOrg, GetStack, GetTile, ListAnnotations, ListDomains, ListDomainsByTile, ListGraphGroups, ListMetrics, ListNodePositions, ListOpenCronRuns, ListProvisionsByConsumer, ListResourcesByEnv, ListStagedByEnv, ListTilesByEnv, ListVariables
handlers/web/handler/project.buildStackGraph -> BindingsForConsumer, CountStagedByEnv, GetOrg, GetTile, HomeEnvironment, ListAnnotations, ListDomains, ListDomainsByTile, ListEnvironmentsByStack, ListGraphGroups, ListNodePositions, ListProvisionsByConsumer, ListResourcesByEnv, ListTilesByEnv, ListVariables
handlers/web/handler/project.commitLog -> GetConnector, GetOrg, ListConfigPlans, ListDeploymentsByTile, ListEnvironmentsByStack, ListTilesByEnv
handlers/web/handler/project.compareEnv -> GetEnvironment, GetOrg, GetStack, ListEnvironmentsByStack, ListIntended
handlers/web/handler/project.compareStack -> GetOrg, ListEnvironmentsByStack, ListIntended
handlers/web/handler/project.defaultEnv -> ListEnvironmentsByStack
handlers/web/handler/project.envColorsByID -> GetOrg
handlers/web/handler/project.envColorsBySlug -> GetOrg, ListEnvironmentsByStack
handlers/web/handler/project.envDeployments -> ListDeploymentsByTile, ListTilesByEnv
handlers/web/handler/project.envFromForm -> GetEnvironment, ListEnvironmentsByStack
handlers/web/handler/project.envSettingsURL -> GetOrg
handlers/web/handler/project.envVarCards -> GetEnvironment, GetOrg, GetStack, ListVariables
handlers/web/handler/project.envVariables -> ListEnvironmentsByStack, ListVariables
handlers/web/handler/project.fetchCommits -> GetConnector, ListEnvironmentsByStack, ListTilesByEnv
handlers/web/handler/project.fillOrg -> GetOrg
handlers/web/handler/project.githubConnectors -> ListConnectorsByOrg
handlers/web/handler/project.loadCommits -> GetConnector, ListEnvironmentsByStack, ListTilesByEnv
handlers/web/handler/project.loadEnvForAnnotation -> GetEnvironment
handlers/web/handler/project.loadPlan -> GetConfigPlan, GetOrg, GetStack
handlers/web/handler/project.loadStack -> GetOrg, GetStack
handlers/web/handler/project.loadStagingEnv -> GetEnvironment, GetOrg, GetStack
handlers/web/handler/project.olderCommit -> GetConnector
handlers/web/handler/project.releaseView -> GetConnector, GetOrg, ListConfigPlans, ListDeploymentsByTile, ListEnvironmentsByStack, ListTilesByEnv
handlers/web/handler/project.renderCompare -> GetConnector, GetOrg, ListConfigPlans, ListDeploymentsByTile, ListEnvironmentsByStack, ListIntended, ListTilesByEnv
handlers/web/handler/project.renderEnvVars -> GetOrg, ListAuditEvents, ListEnvironmentsByStack, ListTilesByEnv, ListVariables
handlers/web/handler/project.renderStackVars -> GetOrg, LatestSettledConfigPlan, ListAuditEvents, ListEnvironmentsByStack, ListSecretLinks, ListVariables
handlers/web/handler/project.resolveSlugs -> GetEnvironmentBySlug, GetOrgBySlug, GetStackBySlug
handlers/web/handler/project.resolveStackSlugs -> GetOrgBySlug, GetStackBySlug
handlers/web/handler/project.settingsEnv -> GetEnvironmentBySlug, GetOrgBySlug, GetStackBySlug
handlers/web/handler/project.settingsSection -> GetOrg
handlers/web/handler/project.settingsStack -> GetOrgBySlug, GetStackBySlug
handlers/web/handler/project.settingsURL -> GetOrg
handlers/web/handler/project.sharedRefs -> BindingsForConsumer, GetTile, ListDomainsByTile, ListProvisionsByConsumer, ListResourcesByEnv
handlers/web/handler/project.skippedRungFor -> ListDeploymentsByTile, ListEnvironmentsByStack, ListTilesByEnv
handlers/web/handler/project.stackVarCards -> ListTilesByEnv, ListVariables
handlers/web/handler/project.stagedMarkers -> ListStagedByEnv
handlers/web/handler/search.Search -> ListBackups, ListConfigPlans, ListConnectors, ListDomains, ListEnvironmentsByStack, ListOrgMembers, ListResourcesByEnv, ListStacks, ListTiles, ListVariableNames
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
handlers/web/handler/server.Move -> GetTile
handlers/web/handler/server.MoveForm -> GetTile
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
