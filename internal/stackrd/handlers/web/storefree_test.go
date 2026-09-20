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
// 253 functions remain. Domain slices done: environments and variables,
// stacks and tiles, organizations and membership, then domains, storage,
// provisions, config plans and deployments.
//
// The order of work, decided with the dev: components/metrics.templ first (a
// TEMPLATE reading the store is the worst of them), then the domains that
// already have a service to move into, then page assembly — which becomes one
// view method per page family, never one forwarder per store method.
const stillStoreReading = `
handlers/api/handler/health.Health -> Health
handlers/api/v1.KeyAuth -> GetAPIKeyByHash, GetUserByID
handlers/api/v1.Register -> GetServer
handlers/api/v1.attachProvision -> GetProvision
handlers/api/v1.checkConnector -> GetConnector
handlers/api/v1.createInvite -> CreateInvite
handlers/api/v1.createRegistry -> CreateRegistry
handlers/api/v1.createRegistryCredential -> GetManagedRegistry
handlers/api/v1.deleteBackup -> DeleteBackup, GetBackup
handlers/api/v1.deleteDestination -> GetBackupDestination
handlers/api/v1.deleteDomain -> GetDomain
handlers/api/v1.deleteInvite -> DeleteInvite, GetInvite
handlers/api/v1.deleteRegistry -> DeleteRegistry, GetRegistry
handlers/api/v1.deleteRegistryTag -> GetManagedRegistry
handlers/api/v1.deleteStoragePath -> DeleteStoragePath, GetStoragePath
handlers/api/v1.getRestore -> GetBackup
handlers/api/v1.listAppResources -> BindingsForConsumer, GetResource, ListOutputs
handlers/api/v1.listBackupRuns -> GetBackup, ListBackupRuns
handlers/api/v1.listBackups -> ListBackupsByTile
handlers/api/v1.listRegistries -> ListRegistries
handlers/api/v1.listRegistryCredentials -> ListOrgRegistryCredentials
handlers/api/v1.listRegistryImages -> GetManagedRegistry
handlers/api/v1.listRegistryTags -> GetManagedRegistry
handlers/api/v1.loadBackup -> GetBackup
handlers/api/v1.newInvite -> CreateInvite
handlers/api/v1.patchBackup -> GetBackup
handlers/api/v1.patchDestination -> GetBackupDestination, UpdateBackupDestination
handlers/api/v1.patchDomain -> GetDomain
handlers/api/v1.patchRegistry -> GetRegistry, UpdateRegistry
handlers/api/v1.patchSettingsFor -> GetServer
handlers/api/v1.probeStorage -> UpdateStorage
handlers/api/v1.requireOrgRegistry -> GetManagedRegistry
handlers/api/v1.requireTile -> GetTileBySlug
handlers/api/v1.resolveSettingsTarget -> GetServer
handlers/api/v1.restoreBackup -> GetBackup, GetBackupRun
handlers/api/v1.runBackup -> GetBackup
handlers/api/v1.settingsFor -> GetServer
handlers/api/v1.tileByPath -> GetTileBySlug
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
handlers/web/handler/app.Attach -> GetServerByNodeID, ListCronRuns, OpenCronRun, UpdateTile
handlers/web/handler/app.AttachProvision -> GetProvision
handlers/web/handler/app.Connectors -> ListConnectorsByOrg
handlers/web/handler/app.CreateAutoDomain -> GetServerByNodeID, ListCronRuns, OpenCronRun
handlers/web/handler/app.CreateDomain -> GetServerByNodeID, ListCronRuns, ListStagedByEnv, OpenCronRun
handlers/web/handler/app.DeleteDomain -> GetServerByNodeID, ListCronRuns, ListStagedByEnv, OpenCronRun
handlers/web/handler/app.DeleteVar -> ListAuditEvents, ListStagedByEnv
handlers/web/handler/app.Detail -> GetServerByNodeID, ListCronRuns, OpenCronRun
handlers/web/handler/app.Panel -> GetServerByNodeID, ListCronRuns, OpenCronRun
handlers/web/handler/app.PanelContent -> GetServerByNodeID, ListCronRuns, OpenCronRun
handlers/web/handler/app.PanelHeader -> OpenCronRun
handlers/web/handler/app.Restart -> OpenCronRun
handlers/web/handler/app.RunNow -> OpenCronRun
handlers/web/handler/app.RunsLogsStream -> ListCronRuns
handlers/web/handler/app.SaveEnv -> GetServerByNodeID, ListCronRuns, OpenCronRun, UpdateTile
handlers/web/handler/app.SaveSecretVar -> ListAuditEvents, ListStagedByEnv
handlers/web/handler/app.SaveSettings -> GetServerByNodeID, ListCronRuns, OpenCronRun
handlers/web/handler/app.SetDomainCert -> GetServerByNodeID, ListCronRuns, OpenCronRun
handlers/web/handler/app.Stop -> OpenCronRun
handlers/web/handler/app.StopRun -> OpenCronRun
handlers/web/handler/app.ToggleCron -> OpenCronRun
handlers/web/handler/app.ToggleDomainHTTPS -> GetServerByNodeID, ListCronRuns, ListStagedByEnv, OpenCronRun
handlers/web/handler/app.Vars -> ListAuditEvents, ListStagedByEnv
handlers/web/handler/app.currentDesiredDomains -> ListStagedByEnv
handlers/web/handler/app.currentDesiredEnv -> ListStagedByEnv
handlers/web/handler/app.headerDone -> OpenCronRun
handlers/web/handler/app.loadTab -> GetServerByNodeID, ListCronRuns, OpenCronRun
handlers/web/handler/app.openRun -> OpenCronRun
handlers/web/handler/app.panelDone -> GetServerByNodeID, ListCronRuns, OpenCronRun
handlers/web/handler/app.placementOf -> GetServerByNodeID
handlers/web/handler/app.stageDomains -> ListStagedByEnv
handlers/web/handler/auth/invite.Page -> GetInvite, MarkInviteUsed, UpsertOrgMember
handlers/web/handler/auth/invite.Submit -> GetInvite, MarkInviteUsed, UpsertOrgMember
handlers/web/handler/auth/invite.join -> MarkInviteUsed, UpsertOrgMember
handlers/web/handler/auth/invite.loadInvite -> GetInvite
handlers/web/handler/backups.Create -> GetBackupDestination, ListBackupRuns, ListBackupsByTile
handlers/web/handler/backups.Delete -> GetBackup, GetBackupDestination, ListBackupRuns, ListBackupsByTile
handlers/web/handler/backups.History -> GetBackup, GetBackupDestination, ListBackupRuns
handlers/web/handler/backups.Panel -> GetBackupDestination, ListBackupRuns, ListBackupsByTile
handlers/web/handler/backups.Restore -> GetBackup, GetBackupDestination, ListBackupRuns, ListBackupsByTile
handlers/web/handler/backups.Run -> GetBackup, GetBackupDestination, ListBackupRuns, ListBackupsByTile
handlers/web/handler/backups.Save -> GetBackup, GetBackupDestination, ListBackupRuns, ListBackupsByTile
handlers/web/handler/backups.configView -> GetBackupDestination, ListBackupRuns
handlers/web/handler/backups.load -> GetBackupDestination, ListBackupRuns, ListBackupsByTile
handlers/web/handler/backups.loadBackup -> GetBackup
handlers/web/handler/backups.render -> GetBackupDestination, ListBackupRuns, ListBackupsByTile
handlers/web/handler/db.ForkProvision -> GetProvision
handlers/web/handler/deployment.Stream -> GetDeployment
handlers/web/handler/notification.Badge -> CountUnreadNotifications
handlers/web/handler/notification.Clear -> DeleteAllNotifications
handlers/web/handler/notification.MarkAllRead -> MarkAllNotificationsRead
handlers/web/handler/notification.Page -> ListNotifications, MarkAllNotificationsRead
handlers/web/handler/org.ApproveOrgPlan -> GetOrgConfigPlan
handlers/web/handler/org.ConnectorBranches -> GetConnector
handlers/web/handler/org.ConnectorFileExists -> GetConnector
handlers/web/handler/org.CreateRegistryCredential -> GetManagedRegistry, ListOrgRegistryCredentials
handlers/web/handler/org.Delete -> DeleteOrg
handlers/web/handler/org.DeleteAnnotation -> DeleteOrg
handlers/web/handler/org.DeleteHomeAnnotation -> DeleteOrg
handlers/web/handler/org.DeleteInvite -> DeleteInvite, GetInvite
handlers/web/handler/org.DeleteOrgVar -> ListAuditEvents
handlers/web/handler/org.Graph -> ListAnnotations, ListConnectorsByOrg, ListGraphGroups, ListNodePositions
handlers/web/handler/org.GraphStatus -> ListAnnotations, ListConnectorsByOrg, ListGraphGroups, ListNodePositions
handlers/web/handler/org.Home -> ListAnnotations, ListGraphGroups, ListNodePositions
handlers/web/handler/org.HomeStatus -> ListAnnotations, ListGraphGroups, ListNodePositions
handlers/web/handler/org.OrgPlanView -> GetOrgConfigPlan, LatestWorkItem
handlers/web/handler/org.RegistryImages -> GetManagedRegistry
handlers/web/handler/org.ReinviteMember -> DeleteInvite, GetInvite
handlers/web/handler/org.RejectOrgPlan -> GetOrgConfigPlan
handlers/web/handler/org.Rename -> UpdateOrg
handlers/web/handler/org.ResendInvite -> GetInvite
handlers/web/handler/org.ResetHomeNodePositions -> DeleteNodePositions
handlers/web/handler/org.ResetNodePositions -> DeleteNodePositions
handlers/web/handler/org.SaveEnvColor -> UpdateOrg
handlers/web/handler/org.SaveHomeNodePosition -> SaveNodePositions
handlers/web/handler/org.SaveNodePosition -> SaveNodePositions
handlers/web/handler/org.SaveOrgConfig -> GetConnector, UpdateOrg
handlers/web/handler/org.SaveOrgVar -> ListAuditEvents
handlers/web/handler/org.SetPlanInput -> GetOrgConfigPlan, UpsertVariable
handlers/web/handler/org.SettingsConfig -> ListConnectorsByOrg
handlers/web/handler/org.SettingsMembers -> ListUsers
handlers/web/handler/org.SettingsRegistry -> GetManagedRegistry, ListOrgRegistryCredentials
handlers/web/handler/org.SettingsVariables -> ListAuditEvents
handlers/web/handler/org.Setup -> ListConnectorsByOrg, ListUsers
handlers/web/handler/org.SetupApprovePlan -> GetOrgConfigPlan
handlers/web/handler/org.SetupConfigPlan -> GetOrgConfigPlan, LatestWorkItem
handlers/web/handler/org.SetupDone -> CreateDomainResource, UpdateOrg
handlers/web/handler/org.SetupMode -> UpdateOrg
handlers/web/handler/org.SetupRejectPlan -> GetOrgConfigPlan
handlers/web/handler/org.VarsPanel -> ListAuditEvents
handlers/web/handler/org.addCandidates -> ListUsers
handlers/web/handler/org.approvePlan -> GetOrgConfigPlan
handlers/web/handler/org.buildOrgGraph -> ListAnnotations, ListConnectorsByOrg, ListGraphGroups, ListNodePositions
handlers/web/handler/org.buildOrgsGraph -> ListAnnotations, ListGraphGroups, ListNodePositions
handlers/web/handler/org.ensureDefaultDomain -> CreateDomainResource
handlers/web/handler/org.githubConnectors -> ListConnectorsByOrg
handlers/web/handler/org.loadOrgPlan -> GetOrgConfigPlan
handlers/web/handler/org.orgPlanWork -> LatestWorkItem
handlers/web/handler/org.ownedInvite -> GetInvite
handlers/web/handler/org.pickerConnector -> GetConnector
handlers/web/handler/org.rejectPlan -> GetOrgConfigPlan
handlers/web/handler/org.renderRegistry -> GetManagedRegistry, ListOrgRegistryCredentials
handlers/web/handler/org.renderVars -> ListAuditEvents
handlers/web/handler/org.setupPlan -> GetOrgConfigPlan
handlers/web/handler/org.setupSummary -> ListConnectorsByOrg
handlers/web/handler/prhook.Hook -> GetConnector, SetSetting
handlers/web/handler/prhook.HookConnector -> GetConnector, SetSetting
handlers/web/handler/prhook.planConfigs -> GetConnector
handlers/web/handler/prhook.updatePlanComment -> GetConnector, SetSetting
handlers/web/handler/project.ApprovePlan -> GetConfigPlan
handlers/web/handler/project.CopyEnv -> GetConnector, ListIntended
handlers/web/handler/project.CreateDB -> GetEnvironment
handlers/web/handler/project.CreateTile -> GetEnvironment
handlers/web/handler/project.DeleteEnvVar -> ListAuditEvents
handlers/web/handler/project.DeleteStackVar -> LatestSettledConfigPlan, ListAuditEvents, ListSecretLinks
handlers/web/handler/project.EnvCompare -> GetConnector, ListIntended
handlers/web/handler/project.EnvVarsPanel -> ListAuditEvents
handlers/web/handler/project.Graph -> BindingsForConsumer, CountStagedByEnv, GetConnector, LatestConfigPlan, ListAnnotations, ListGraphGroups, ListIntended, ListMetrics, ListNodePositions, ListOpenCronRuns, ListResourcesByEnv, ListStagedByEnv
handlers/web/handler/project.GraphStatus -> BindingsForConsumer, ListAnnotations, ListGraphGroups, ListMetrics, ListNodePositions, ListOpenCronRuns, ListResourcesByEnv, ListStagedByEnv
handlers/web/handler/project.MarkIntended -> GetConnector, ListIntended, SetIntended
handlers/web/handler/project.PlanView -> GetConfigPlan, GetServerByNodeID, LatestWorkItem
handlers/web/handler/project.PromoteDialogue -> GetConnector
handlers/web/handler/project.RedirectStack -> GetOrgBySlug
handlers/web/handler/project.RejectPlan -> GetConfigPlan
handlers/web/handler/project.Releases -> GetConnector
handlers/web/handler/project.Repos -> ListConnectorsByOrg
handlers/web/handler/project.ResetNodePositions -> DeleteNodePositions
handlers/web/handler/project.ResetStackNodePositions -> DeleteNodePositions
handlers/web/handler/project.RevokeStackLink -> ListSecretLinks
handlers/web/handler/project.SaveEnvVar -> ListAuditEvents
handlers/web/handler/project.SaveNodePosition -> SaveNodePositions
handlers/web/handler/project.SaveStackNodePosition -> SaveNodePositions
handlers/web/handler/project.SaveStackVar -> LatestSettledConfigPlan, ListAuditEvents, ListSecretLinks
handlers/web/handler/project.SetPlanInput -> GetConfigPlan
handlers/web/handler/project.SettingsConfig -> ListConnectorsByOrg
handlers/web/handler/project.SettingsEnvironment -> ListAuditEvents
handlers/web/handler/project.SettingsPREnv -> ListConnectorsByOrg
handlers/web/handler/project.SettingsVariables -> LatestSettledConfigPlan, ListAuditEvents, ListSecretLinks
handlers/web/handler/project.StackGraph -> BindingsForConsumer, CountStagedByEnv, GetConnector, HomeEnvironment, LatestConfigPlan, ListAnnotations, ListGraphGroups, ListIntended, ListNodePositions, ListResourcesByEnv
handlers/web/handler/project.StackGraphStatus -> BindingsForConsumer, CountStagedByEnv, HomeEnvironment, ListAnnotations, ListGraphGroups, ListNodePositions, ListResourcesByEnv
handlers/web/handler/project.StackVarsPanel -> LatestSettledConfigPlan, ListAuditEvents, ListSecretLinks
handlers/web/handler/project.StagingDiscard -> DeleteStagedByEnv
handlers/web/handler/project.StagingDiscardOne -> CountStagedByEnv, DeleteStagedChange, GetStagedChange
handlers/web/handler/project.StagingReview -> ListStagedByEnv
handlers/web/handler/project.buildGraph -> BindingsForConsumer, ListAnnotations, ListGraphGroups, ListMetrics, ListNodePositions, ListOpenCronRuns, ListResourcesByEnv, ListStagedByEnv
handlers/web/handler/project.buildStackGraph -> BindingsForConsumer, CountStagedByEnv, HomeEnvironment, ListAnnotations, ListGraphGroups, ListNodePositions, ListResourcesByEnv
handlers/web/handler/project.commitLog -> GetConnector
handlers/web/handler/project.compareEnv -> ListIntended
handlers/web/handler/project.compareStack -> ListIntended
handlers/web/handler/project.envFromForm -> GetEnvironment
handlers/web/handler/project.fetchCommits -> GetConnector
handlers/web/handler/project.githubConnectors -> ListConnectorsByOrg
handlers/web/handler/project.loadCommits -> GetConnector
handlers/web/handler/project.loadPlan -> GetConfigPlan
handlers/web/handler/project.olderCommit -> GetConnector
handlers/web/handler/project.releaseView -> GetConnector
handlers/web/handler/project.renderCompare -> GetConnector, ListIntended
handlers/web/handler/project.renderEnvVars -> ListAuditEvents
handlers/web/handler/project.renderStackVars -> LatestSettledConfigPlan, ListAuditEvents, ListSecretLinks
handlers/web/handler/project.sharedRefs -> BindingsForConsumer, ListResourcesByEnv
handlers/web/handler/project.stagedMarkers -> ListStagedByEnv
handlers/web/handler/search.Search -> ListBackups, ListConnectors, ListResourcesByEnv, ListVariableNames
handlers/web/handler/server.Activate -> GetServer
handlers/web/handler/server.CreateDomainResource -> GetServer
handlers/web/handler/server.CreateVolume -> GetServer
handlers/web/handler/server.DeleteStoragePath -> DeleteStoragePath, GetStoragePath
handlers/web/handler/server.DeleteVolume -> GetServer
handlers/web/handler/server.Detail -> GetServer, ListMetrics
handlers/web/handler/server.Drain -> GetServer
handlers/web/handler/server.DrainForm -> GetServer
handlers/web/handler/server.Host -> GetServer
handlers/web/handler/server.JoinScript -> GetManagedRegistry
handlers/web/handler/server.NewJoinKey -> GetServer
handlers/web/handler/server.ProbeStorage -> UpdateStorage
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
handlers/web/handler/settings.DeleteConnector -> DeleteConnector, GetConnector
handlers/web/handler/settings.DeleteDestination -> GetBackupDestination
handlers/web/handler/settings.DeletePanelBackup -> DeleteBackup, ListBackupRuns, ListBackups
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
handlers/web/handler/settings.panelBackup -> ListBackupRuns, ListBackups
`
