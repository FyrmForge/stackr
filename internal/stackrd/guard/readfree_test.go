package guard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// The rule: nothing outside internal/stackrd/service READS repo.Store either.
//
// This is the third guard, and the one that closes the shape. The first
// (handlers/web/storefree_test.go) stopped handlers reading. The second
// (writefree_test.go) stopped everything writing. Between them sat a gap
// nobody had ever named: a non-handler read. That gap is exactly config/ and
// infra/, and the work queue lived in it — LatestWorkItem read straight off
// the store from infra/backup, infra/jobs and infra/volmove, each rebuilding
// its own idea of "is my thing still in flight" from the raw row.
//
// The rule is deliberately not "reads that carry a domain rule". Drawing that
// line means someone has to judge, per call site, whether a lookup is plain
// enough to leave alone — and that judgement is the drift. A row fetched
// direct is a row nobody can add to later: the day a lookup needs a second
// table joined onto it, or a field the store does not hold, the addendum has
// to be written at every call site instead of one.
//
// So: every read goes through a service too, and the seam exists before it is
// needed rather than after.
//
// stillReading is the worklist. It shrinks; it does not grow.

// readMethods is the complement of writeMethods over the same interface:
// every repo.Store method that is not a mutation. Deriving it by subtraction
// rather than by a verb list of its own means the two guards can never both
// miss a method — a new store method is in one set or the other the moment it
// is declared.
func readMethods(t *testing.T, repoFile string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, repoFile, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", repoFile, err)
	}
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Store" {
			return true
		}
		it, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			return true
		}
		for _, m := range it.Methods.List {
			for _, nm := range m.Names {
				if !isWriteVerb(nm.Name) || notAWrite[nm.Name] {
					out[nm.Name] = true
				}
			}
		}
		return false
	})
	if len(out) == 0 {
		t.Fatal("found no read methods on repo.Store — the parse is wrong, not the interface")
	}
	return out
}

func TestNothingOutsideServiceReadsTheStore(t *testing.T) {
	root := "../../.."
	reads := readMethods(t, filepath.Join(root, "internal/stackrd/store/repo/repo.go"))
	checkWorklist(t, "read", "stillReading", stillReading, storeCalls(t, root, reads))
}

// stillReading is the worklist: every store read outside service/, as
// `<count> <package>/<Method>`. Seeded from the scan, not typed.
const stillReading = `
1 cmd/stackrd/GetServer
1 cmd/stackrd/GetSetting
1 cmd/stackrd/GetStack
1 cmd/stackrd/GetUserByID
1 cmd/stackrd/ListDomainResources
4 internal/stackrd/config/envops/GetTile
1 internal/stackrd/config/envops/ListDomainsByTile
1 internal/stackrd/config/envops/ListEnvironmentsByStack
1 internal/stackrd/config/envops/ListNodePositions
2 internal/stackrd/config/envops/ListProvisionsByConsumer
2 internal/stackrd/config/envops/ListProvisionsByEnv
1 internal/stackrd/config/envops/ListResourcesByEnv
2 internal/stackrd/config/envops/ListTilesByEnv
3 internal/stackrd/config/envops/ListVariables
1 internal/stackrd/config/orgconf/GetConnector
1 internal/stackrd/config/orgconf/GetOrg
1 internal/stackrd/config/orgconf/GetOrgBySlug
1 internal/stackrd/config/orgconf/GetOrgConfigPlan
3 internal/stackrd/config/orgconf/GetStackBySlug
3 internal/stackrd/config/orgconf/ListDomainResources
1 internal/stackrd/config/orgconf/ListEnvironmentsByStack
8 internal/stackrd/config/orgconf/ListStacksByOrg
2 internal/stackrd/config/orgconf/ListStorage
2 internal/stackrd/config/orgconf/ListTilesByStack
5 internal/stackrd/config/orgconf/ListVariables
1 internal/stackrd/config/settings/GetEnvironment
1 internal/stackrd/config/settings/GetOrg
2 internal/stackrd/config/settings/GetServer
1 internal/stackrd/config/settings/GetStack
1 internal/stackrd/config/sharelink/ListVariables
1 internal/stackrd/config/stackconf/GetBackupDestination
1 internal/stackrd/config/stackconf/GetConfigPlan
3 internal/stackrd/config/stackconf/GetConnector
1 internal/stackrd/config/stackconf/GetEnvironment
15 internal/stackrd/config/stackconf/GetEnvironmentBySlug
5 internal/stackrd/config/stackconf/GetOrg
1 internal/stackrd/config/stackconf/GetSetting
3 internal/stackrd/config/stackconf/GetStack
1 internal/stackrd/config/stackconf/GetStackBySlug
4 internal/stackrd/config/stackconf/GetTile
9 internal/stackrd/config/stackconf/GetTileBySlug
1 internal/stackrd/config/stackconf/HomeEnvironment
2 internal/stackrd/config/stackconf/ListBackupsByTile
1 internal/stackrd/config/stackconf/ListConnectorsByOrg
1 internal/stackrd/config/stackconf/ListDeploymentsByTile
3 internal/stackrd/config/stackconf/ListDomainResources
1 internal/stackrd/config/stackconf/ListDomains
3 internal/stackrd/config/stackconf/ListDomainsByTile
12 internal/stackrd/config/stackconf/ListEnvironmentsByStack
1 internal/stackrd/config/stackconf/ListOrgs
1 internal/stackrd/config/stackconf/ListProvisionsByEnv
1 internal/stackrd/config/stackconf/ListProvisionsByInstance
4 internal/stackrd/config/stackconf/ListResourcesByEnv
1 internal/stackrd/config/stackconf/ListStacksByOrg
1 internal/stackrd/config/stackconf/ListStagedByStack
5 internal/stackrd/config/stackconf/ListTilesByEnv
1 internal/stackrd/config/stackconf/ListTilesByStack
7 internal/stackrd/config/stackconf/ListVariables
1 internal/stackrd/config/staging/ListStagedByEnv
2 internal/stackrd/config/varref/BindingsForConsumer
1 internal/stackrd/config/varref/GetEnvironment
1 internal/stackrd/config/varref/GetOrgStorageBySlug
1 internal/stackrd/config/varref/GetResource
3 internal/stackrd/config/varref/GetStack
4 internal/stackrd/config/varref/GetTile
1 internal/stackrd/config/varref/ListDomainsByTile
2 internal/stackrd/config/varref/ListOutputs
1 internal/stackrd/config/varref/ListResourcesByEnv
1 internal/stackrd/config/varref/ListStacksByOrg
2 internal/stackrd/config/varref/ListTilesByEnv
2 internal/stackrd/config/varref/ListTilesByStack
7 internal/stackrd/config/varref/ListVariables
1 internal/stackrd/handlers/api/handler/health/Health
3 internal/stackrd/handlers/middleware/GetOrg
2 internal/stackrd/handlers/middleware/GetOrgBySlug
4 internal/stackrd/handlers/middleware/GetOrgMember
2 internal/stackrd/handlers/middleware/GetStack
1 internal/stackrd/handlers/middleware/ListOrgs
1 internal/stackrd/handlers/middleware/ListOrgsForUser
1 internal/stackrd/handlers/web/components/GetOrg
1 internal/stackrd/handlers/web/components/GetStack
1 internal/stackrd/handlers/web/components/ListEnvironmentsByStack
2 internal/stackrd/handlers/web/CountUsers
1 internal/stackrd/handlers/web/GetEnvironment
1 internal/stackrd/handlers/web/GetEnvironmentBySlug
2 internal/stackrd/handlers/web/GetManagedRegistry
1 internal/stackrd/handlers/web/GetOrg
2 internal/stackrd/handlers/web/GetOrgBySlug
1 internal/stackrd/handlers/web/GetOrgMember
1 internal/stackrd/handlers/web/GetOrgRegistryCredentialByHash
1 internal/stackrd/handlers/web/GetStack
1 internal/stackrd/handlers/web/GetStackBySlug
1 internal/stackrd/handlers/web/GetTile
1 internal/stackrd/handlers/web/GetTileBySlug
1 internal/stackrd/handlers/web/GetUserByID
1 internal/stackrd/infra/agent/GetManagedRegistry
5 internal/stackrd/infra/backup/GetBackup
3 internal/stackrd/infra/backup/GetBackupDestination
4 internal/stackrd/infra/backup/GetBackupRun
2 internal/stackrd/infra/backup/GetStack
5 internal/stackrd/infra/backup/GetTile
1 internal/stackrd/infra/backup/LatestWorkItem
1 internal/stackrd/infra/backup/ListBackupDestinations
1 internal/stackrd/infra/backup/ListBackups
1 internal/stackrd/infra/cigate/GetConnector
1 internal/stackrd/infra/cigate/GetTile
1 internal/stackrd/infra/cigate/ListDeploymentsByStatus
1 internal/stackrd/infra/cluster/GetServer
4 internal/stackrd/infra/deploy/GetDeployment
1 internal/stackrd/infra/deploy/GetManagedRegistry
1 internal/stackrd/infra/deploy/GetTile
1 internal/stackrd/infra/deploy/GetTileBySlug
1 internal/stackrd/infra/deploy/ListDeploymentsByStatus
2 internal/stackrd/infra/deploy/ListDeploymentsByTile
1 internal/stackrd/infra/deploy/ListStacksByOrg
2 internal/stackrd/infra/deploy/ListTilesByEnv
2 internal/stackrd/infra/deploy/ListTilesByStack
2 internal/stackrd/infra/envnet/GetEnvironment
1 internal/stackrd/infra/envnet/GetOrg
1 internal/stackrd/infra/envnet/GetStack
1 internal/stackrd/infra/envnet/ListEnvironmentsByStack
2 internal/stackrd/infra/githubapp/GetConnector
1 internal/stackrd/infra/githubapp/GetEnvironment
1 internal/stackrd/infra/githubapp/GetSetting
1 internal/stackrd/infra/githubapp/GetStack
1 internal/stackrd/infra/githubapp/ListConnectorsByOrg
1 internal/stackrd/infra/githubapp/ListDeploymentsByTile
1 internal/stackrd/infra/githubapp/ListDomainsByTile
1 internal/stackrd/infra/githubapp/ListTilesByEnv
3 internal/stackrd/infra/jobs/GetCronRun
3 internal/stackrd/infra/jobs/GetTile
1 internal/stackrd/infra/jobs/GetTileBySlug
1 internal/stackrd/infra/jobs/LatestWorkItem
1 internal/stackrd/infra/jobs/ListDeploymentsByTile
1 internal/stackrd/infra/jobs/ListTiles
1 internal/stackrd/infra/managedtiles/GetEnvironment
4 internal/stackrd/infra/managedtiles/GetEnvironmentBySlug
2 internal/stackrd/infra/managedtiles/GetOrg
2 internal/stackrd/infra/managedtiles/GetOrgBySlug
2 internal/stackrd/infra/managedtiles/GetStack
5 internal/stackrd/infra/managedtiles/GetStackBySlug
8 internal/stackrd/infra/managedtiles/GetTile
2 internal/stackrd/infra/managedtiles/GetTileBySlug
1 internal/stackrd/infra/managedtiles/ListDomainsByTile
1 internal/stackrd/infra/managedtiles/ListProvisionsByConsumer
3 internal/stackrd/infra/managedtiles/ListProvisionsByEnv
7 internal/stackrd/infra/managedtiles/ListProvisionsByInstance
2 internal/stackrd/infra/managedtiles/ListResourcesByEnv
3 internal/stackrd/infra/managedtiles/ListStacksByOrg
1 internal/stackrd/infra/managedtiles/ListTiles
3 internal/stackrd/infra/managedtiles/ListTilesByStack
1 internal/stackrd/infra/managedtiles/ListVariables
1 internal/stackrd/infra/metrics/GetTile
1 internal/stackrd/infra/metrics/ListResourcesByProvider
2 internal/stackrd/infra/metrics/ListTiles
2 internal/stackrd/infra/netpool/ClaimedNetworks
1 internal/stackrd/infra/netpool/EnvironmentsWithoutNetwork
1 internal/stackrd/infra/netpool/GetEnvironment
2 internal/stackrd/infra/nodes/GetJoinKey
2 internal/stackrd/infra/nodes/GetServer
1 internal/stackrd/infra/nodes/GetServerByNodeID
1 internal/stackrd/infra/nodes/LatestJoinKey
1 internal/stackrd/infra/nodes/ListMetrics
4 internal/stackrd/infra/nodes/ListServers
1 internal/stackrd/infra/placement/GetServer
1 internal/stackrd/infra/placement/GetStorageBySlug
1 internal/stackrd/infra/placement/GetTile
1 internal/stackrd/infra/placement/ListTilesByEnv
1 internal/stackrd/infra/proxy/GetManagedRegistry
7 internal/stackrd/infra/proxy/GetSetting
1 internal/stackrd/infra/proxy/GetStack
1 internal/stackrd/infra/proxy/GetStackBySlug
1 internal/stackrd/infra/proxy/HomeEnvironment
2 internal/stackrd/infra/proxy/ListDomainResources
1 internal/stackrd/infra/proxy/ListDomainsByTile
1 internal/stackrd/infra/proxy/ListEnvironmentsByStack
2 internal/stackrd/infra/proxy/ListStacks
1 internal/stackrd/infra/proxy/ListTiles
2 internal/stackrd/infra/registry/GetManagedRegistry
1 internal/stackrd/infra/registry/GetOrg
1 internal/stackrd/infra/registry/GetStack
1 internal/stackrd/infra/registry/ListDeploymentsByTile
1 internal/stackrd/infra/registry/ListOrgRegistryCredentials
1 internal/stackrd/infra/registry/ListStacksByOrg
1 internal/stackrd/infra/registry/ListTilesByStack
1 internal/stackrd/infra/storagetiles/GetOrgStorageBySlug
1 internal/stackrd/infra/storagetiles/GetStack
1 internal/stackrd/infra/storagetiles/GetStorageBySlug
1 internal/stackrd/infra/storagetiles/ListStoragePaths
1 internal/stackrd/infra/volmove/GetDeployment
3 internal/stackrd/infra/volmove/GetTile
1 internal/stackrd/infra/volmove/GetWorkItem
2 internal/stackrd/infra/volmove/LatestWorkItem
1 internal/stackrd/infra/volmove/ListTilesByEnv
1 internal/stackrd/infra/workqueue/GetWorkItem
3 internal/stackrd/infra/workqueue/ListWorkItemsByStatus
`
