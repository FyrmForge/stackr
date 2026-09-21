package guard

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

// The rule: nothing outside internal/stackrd/service writes to repo.Store.
//
// This is the write-side twin of handlers/web/storefree_test.go. That one
// stopped handlers from READING the store, and the drift audit that followed
// (docs/plans/service-extraction/09-drift-audit.md) found the obvious hole:
// nobody had ever said anything about writes, and two whole trees — config/
// and infra/ — were writing concepts the services own. 175 call sites.
//
// Why it matters, in one line: a write that does not go through the service
// is a write that skips the service's rules, and the next person to add a
// rule adds it in one place out of two. The domain bugs in D-5 were exactly
// that — the panel refused a wildcard domain with no DNS provider and a YAML
// file did not.
//
// stillWriting is the worklist. It shrinks; it does not grow.

// writeMethods reads the repo.Store interface and returns the subset of its
// methods that mutate. Deriving the list from the interface rather than from
// a regex over call sites means a new store method is classified the moment
// it is declared, not the moment someone notices.
func writeMethods(t *testing.T, repoFile string) map[string]bool {
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
				if isWriteVerb(nm.Name) && !notAWrite[nm.Name] {
					out[nm.Name] = true
				}
			}
		}
		return false
	})
	if len(out) == 0 {
		t.Fatal("found no write methods on repo.Store — the parse is wrong, not the interface")
	}
	return out
}

var writeVerbs = []string{
	"Create", "Update", "Delete", "Set", "Insert", "Upsert", "Save", "Remove",
	"Add", "Mark", "Clear", "Attach", "Detach", "Bind", "Unbind", "Revoke",
	"Record", "Enqueue", "Claim", "Finish", "Fail", "Purge", "Prune", "Rotate",
	"Reset", "Replace", "Touch", "Burn", "Supersede", "Rename", "Requeue",
	"Sweep", "Drop", "Stamp",
}

func isWriteVerb(name string) bool {
	for _, v := range writeVerbs {
		if strings.HasPrefix(name, v) {
			return true
		}
	}
	return false
}

// notAWrite is the false-positive list: methods whose name starts with a
// mutating verb and which only read. Each one is a real reading of the
// method, not a guess.
var notAWrite = map[string]bool{
	// "networks already claimed" — a SELECT over environments.
	"ClaimedNetworks": true,
	// "which bindings does this consumer have" — a SELECT over bindings.
	"BindingsForConsumer": true,
}

// site is one banned call.
type site struct {
	pkg    string
	method string
	line   string
}

// storeWrites walks the tree and reports every call to a write method made on
// something that is a repo.Store.
//
// Two spellings are matched, the same two storefree_test.go learned to match:
// a field — x.store.CreateTile(…) — and a bare identifier that the function
// was handed as a repo.Store — store.CreateTile(…). The second is the one
// that hides: without reading the parameter types it is indistinguishable
// from a call to a function in the same package.
func storeWrites(t *testing.T, root string, writes map[string]bool) []site {
	t.Helper()
	var found []site
	fset := token.NewFileSet()
	// Field names declared repo.Store, collected across the WHOLE tree before
	// the call scan. Per-file was the guard's second hole: a struct declared
	// in one file and used in another carries its store field under a name
	// the second file never sees, so `x.db.CreateTile(…)` reads as a call on
	// something unknown. Names are cheap; a missed write is not.
	fields := map[string]bool{"store": true, "Store": true}
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			if skipDir(p) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_templ.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fl := range st.Fields.List {
				if !isRepoStore(fl.Type) {
					continue
				}
				for _, nm := range fl.Names {
					fields[nm.Name] = true
				}
			}
			return true
		})
		return nil
	})
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			if skipDir(p) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") ||
			strings.HasSuffix(p, "_test.go") ||
			strings.HasSuffix(p, "_templ.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		pkg := filepath.ToSlash(filepath.Dir(rel))

		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			bare := map[string]bool{}
			for _, fl := range fnFields(fd) {
				if !isRepoStore(fl.Type) {
					continue
				}
				for _, nm := range fl.Names {
					bare[nm.Name] = true
				}
			}
			// Locals that ARE the store: `store := a.Planner.Store`. This was
			// the guard's own first hole — sixteen writes in stackconf go
			// through one of these, and the walk filed every one of them as a
			// call to something else entirely. Anything assigned from a
			// selector ending in .Store or .store counts.
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || as.Tok != token.DEFINE || len(as.Lhs) != len(as.Rhs) {
					return true
				}
				for i, rhs := range as.Rhs {
					sel, ok := rhs.(*ast.SelectorExpr)
					if !ok || (sel.Sel.Name != "Store" && sel.Sel.Name != "store") {
						continue
					}
					if id, ok := as.Lhs[i].(*ast.Ident); ok {
						bare[id.Name] = true
					}
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				ce, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := ce.Fun.(*ast.SelectorExpr)
				if !ok || !writes[sel.Sel.Name] {
					return true
				}
				hit := false
				switch x := sel.X.(type) {
				case *ast.SelectorExpr:
					// x.store.Method(…) — a field on a struct.
					hit = fields[x.Sel.Name]
				case *ast.Ident:
					// store.Method(…) — the store as a parameter.
					hit = bare[x.Name]
				}
				if hit {
					found = append(found, site{
						pkg:    pkg,
						method: sel.Sel.Name,
						line:   filepath.ToSlash(rel) + ":" + itoa(fset.Position(ce.Pos()).Line),
					})
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return found
}

// skipDir is the below-the-line list. service/ is where writes belong;
// store/ IS the store; data/ holds checked-out user repos, which are not
// this codebase at all.
func skipDir(p string) bool {
	s := filepath.ToSlash(p)
	switch {
	case strings.HasSuffix(s, "/internal/stackrd/service"):
		return true
	case strings.HasSuffix(s, "/internal/stackrd/store"):
		return true
	case strings.Contains(s, "/data/repos/"):
		return true
	case strings.HasSuffix(s, "/node_modules") || strings.HasSuffix(s, "/.git"):
		return true
	}
	return false
}

func TestNothingOutsideServiceWritesTheStore(t *testing.T) {
	root := "../../.."
	writes := writeMethods(t, filepath.Join(root, "internal/stackrd/store/repo/repo.go"))

	allowed := map[string]int{}
	for _, l := range strings.Split(strings.TrimSpace(stillWriting), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Fields(l)
		if len(f) != 2 {
			t.Fatalf("malformed stillWriting line: %q (want `<count> <pkg>/<Method>`)", l)
		}
		allowed[f[1]] = atoi(t, f[0])
	}

	got := map[string]int{}
	lines := map[string][]string{}
	for _, s := range storeWrites(t, root, writes) {
		k := s.pkg + "/" + s.method
		got[k]++
		lines[k] = append(lines[k], s.line)
	}

	var over, under []string
	for k, n := range got {
		if a, ok := allowed[k]; !ok {
			over = append(over, k+" ("+itoa(n)+" site(s)): "+strings.Join(lines[k], ", "))
		} else if n > a {
			over = append(over, k+" grew from "+itoa(a)+" to "+itoa(n)+": "+strings.Join(lines[k], ", "))
		} else if n < a {
			under = append(under, k+" is down to "+itoa(n)+" from "+itoa(a)+" — lower the count")
		}
	}
	for k, a := range allowed {
		if got[k] == 0 {
			under = append(under, k+" no longer writes ("+itoa(a)+" expected) — delete the line")
		}
	}
	sort.Strings(over)
	sort.Strings(under)

	if len(over) > 0 {
		t.Errorf("%d store write(s) outside service/ that stillWriting does not cover.\n"+
			"A write that skips the service skips the service's rules, and the next rule\n"+
			"gets added in one place out of two. Route it through the service:\n  %s",
			len(over), strings.Join(over, "\n  "))
	}
	if len(under) > 0 {
		t.Errorf("%d stillWriting entr(ies) are stale. The list only shrinks, so shrink it:\n  %s",
			len(under), strings.Join(under, "\n  "))
	}
	t.Logf("%d write site(s) still outside service/", len(storeWrites(t, root, writes)))
}

// fnFields is a function's receiver plus its parameters, flattened.
func fnFields(fd *ast.FuncDecl) []*ast.Field {
	var out []*ast.Field
	if fd.Recv != nil {
		out = append(out, fd.Recv.List...)
	}
	if fd.Type.Params != nil {
		out = append(out, fd.Type.Params.List...)
	}
	return out
}

// isRepoStore reports whether a type expression is repo.Store.
func isRepoStore(x ast.Expr) bool {
	sel, ok := x.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Store" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "repo"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func atoi(t *testing.T, s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("not a count: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// stillWriting is the worklist: every store write outside service/, as
// `<count> <package>/<Method>`. Seeded from the scan, not typed.
const stillWriting = `
1 cmd/stackrd/CreateDomainResource
3 cmd/stackrd/SetSetting
1 internal/stackrd/config/envops/CreateBinding
1 internal/stackrd/config/envops/CreateTile
2 internal/stackrd/config/envops/DeleteProvision
1 internal/stackrd/config/envops/SaveNodePositions
1 internal/stackrd/config/orgconf/CreateDomainResource
1 internal/stackrd/config/orgconf/CreateOrgConfigPlan
1 internal/stackrd/config/orgconf/CreateStorage
1 internal/stackrd/config/orgconf/CreateTile
1 internal/stackrd/config/orgconf/DeleteDomainResource
1 internal/stackrd/config/orgconf/DeleteStack
1 internal/stackrd/config/orgconf/DeleteStorage
1 internal/stackrd/config/orgconf/DeleteTile
1 internal/stackrd/config/orgconf/RenameTile
3 internal/stackrd/config/orgconf/SetOrgConfigPlanError
1 internal/stackrd/config/orgconf/SetOrgConfigPlanStatus
1 internal/stackrd/config/orgconf/SupersedePendingOrgPlans
1 internal/stackrd/config/orgconf/UpdateDomainResource
1 internal/stackrd/config/orgconf/UpdateOrg
1 internal/stackrd/config/orgconf/UpdateStack
1 internal/stackrd/config/orgconf/UpdateStorage
1 internal/stackrd/config/orgconf/UpdateTile
1 internal/stackrd/config/sharelink/BurnDropLink
3 internal/stackrd/config/sharelink/ClaimSecretLink
1 internal/stackrd/config/sharelink/CreateSecretLink
2 internal/stackrd/config/sharelink/TouchSecretLink
1 internal/stackrd/config/stackconf/ClearDeclaredIntended
1 internal/stackrd/config/stackconf/CreateBinding
1 internal/stackrd/config/stackconf/CreateConfigPlan
2 internal/stackrd/config/stackconf/CreateDomain
1 internal/stackrd/config/stackconf/CreateDomainResource
1 internal/stackrd/config/stackconf/CreateTile
2 internal/stackrd/config/stackconf/DeleteBackup
2 internal/stackrd/config/stackconf/DeleteDomain
1 internal/stackrd/config/stackconf/DeleteDomainResource
1 internal/stackrd/config/stackconf/DeleteStagedByEnv
1 internal/stackrd/config/stackconf/SetConfigPlanError
2 internal/stackrd/config/stackconf/SetConfigPlanStatus
1 internal/stackrd/config/stackconf/SetIntended
1 internal/stackrd/config/stackconf/SupersedePendingPlans
1 internal/stackrd/config/stackconf/UpdateDomainResource
2 internal/stackrd/config/stackconf/UpdateProvision
4 internal/stackrd/config/stackconf/UpdateStack
1 internal/stackrd/config/stackconf/UpdateTile
1 internal/stackrd/config/staging/CreateStagedChange
1 internal/stackrd/config/staging/DeleteStagedChange
1 internal/stackrd/handlers/web/TouchOrgRegistryCredential
1 internal/stackrd/infra/backup/CreateBackupRun
2 internal/stackrd/infra/backup/UpdateBackupRun
1 internal/stackrd/infra/githubapp/CreateConnector
1 internal/stackrd/infra/githubapp/UpdateConnector
1 internal/stackrd/infra/jobs/CreateCronRun
1 internal/stackrd/infra/jobs/FinishCronRun
1 internal/stackrd/infra/jobs/PruneCronRuns
2 internal/stackrd/infra/jobs/RecordTileRun
1 internal/stackrd/infra/managedtiles/CreateBinding
3 internal/stackrd/infra/managedtiles/CreateProvision
1 internal/stackrd/infra/managedtiles/CreateResource
1 internal/stackrd/infra/managedtiles/DeleteBinding
1 internal/stackrd/infra/managedtiles/DeleteProvision
2 internal/stackrd/infra/managedtiles/DeleteResource
1 internal/stackrd/infra/managedtiles/DeleteVariable
1 internal/stackrd/infra/managedtiles/SetTileImageDigest
5 internal/stackrd/infra/managedtiles/UpdateProvision
1 internal/stackrd/infra/managedtiles/UpdateResource
1 internal/stackrd/infra/managedtiles/UpdateTile
1 internal/stackrd/infra/managedtiles/UpsertOutput
1 internal/stackrd/infra/managedtiles/UpsertVariable
4 internal/stackrd/infra/metrics/InsertMetric
1 internal/stackrd/infra/metrics/PruneMetrics
1 internal/stackrd/infra/metrics/UpdateTileStatus
1 internal/stackrd/infra/nodes/BurnJoinKey
1 internal/stackrd/infra/nodes/BurnServerJoinKeys
1 internal/stackrd/infra/nodes/CreateJoinKey
2 internal/stackrd/infra/nodes/CreateServer
1 internal/stackrd/infra/nodes/InsertMetric
2 internal/stackrd/infra/nodes/UpdateServer
2 internal/stackrd/infra/placement/SetTileHomeNode
1 internal/stackrd/infra/proxy/SetSetting
1 internal/stackrd/infra/registry/CreateOrgRegistryCredential
1 internal/stackrd/infra/registry/CreateRegistry
1 internal/stackrd/infra/registry/DeleteSystemOrgRegistryCredential
3 internal/stackrd/infra/volmove/SetTileHomeNode
3 internal/stackrd/infra/volmove/UpdateTileStatus
1 internal/stackrd/infra/workqueue/ClaimWorkItem
1 internal/stackrd/infra/workqueue/CreateWorkItem
7 internal/stackrd/infra/workqueue/FinishWorkItem
1 internal/stackrd/infra/workqueue/RequeueWorkItem
2 internal/stackrd/infra/workqueue/SetWorkItemProgress
1 internal/stackrd/infra/workqueue/SupersedeQueuedWorkItems
`

// The audit table has a back door. `store/audit.Record(ctx, store, …)` calls
// AddAuditEvent, so it is a store write — but it is spelled as a call to a
// helper with the store as an argument, which the walk above cannot see. The
// scan found fourteen of them, two in handler packages that the write guard
// would otherwise declare clean.
//
// Decided 2026-09-20: AuditService takes the write and store/audit.Record is
// deleted. Until it is, this second rule keeps the count honest, so the green
// above cannot be read as "the handlers write nothing".
func TestNothingOutsideServiceHandsTheStoreToAudit(t *testing.T) {
	root := "../../.."
	fset := token.NewFileSet()
	got := map[string]int{}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			if skipDir(p) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") ||
			strings.HasSuffix(p, "_test.go") ||
			strings.HasSuffix(p, "_templ.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		ast.Inspect(f, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := ce.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Record" {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != "audit" {
				return true
			}
			got[filepath.ToSlash(filepath.Dir(rel))]++
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking: %v", err)
	}

	allowed := map[string]int{}
	for _, l := range strings.Split(strings.TrimSpace(stillAuditing), "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		f := strings.Fields(l)
		if len(f) != 2 {
			t.Fatalf("malformed stillAuditing line: %q", l)
		}
		allowed[f[1]] = atoi(t, f[0])
	}

	var bad []string
	for pkg, n := range got {
		if a, ok := allowed[pkg]; !ok || n > a {
			bad = append(bad, pkg+": "+itoa(n)+" call(s)")
		}
	}
	for pkg, a := range allowed {
		if got[pkg] < a {
			bad = append(bad, pkg+" is down to "+itoa(got[pkg])+" from "+itoa(a)+" — lower it")
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("audit.Record outside service/ does not match stillAuditing:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// stillAuditing is the audit back door's worklist, by package.
const stillAuditing = `
2 internal/stackrd/config/orgconf
1 internal/stackrd/config/envops
2 internal/stackrd/config/sharelink
2 internal/stackrd/config/stackconf
2 internal/stackrd/handlers/api/v1
3 internal/stackrd/handlers/middleware
1 internal/stackrd/infra/managedtiles
`
