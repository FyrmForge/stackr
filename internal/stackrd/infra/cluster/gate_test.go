package cluster_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// nodeScoped is every runtime method that acts on one container, one image or
// one volume. Each of these lives on exactly one machine, so calling it
// without deciding which machine is a bug that reports success: the postgres
// provisioner ran nine QA rounds' worth of execs against the manager while the
// instance it was provisioning sat on a worker.
//
// This list is the rule. Outside infra/cluster and infra/runtime nothing may
// call one of these on a *runtime.Runtime, and since the test matches by
// method name it does not have to prove the receiver's type: a name from this
// list called on anything else is worth a look anyway.
var nodeScoped = map[string]bool{
	"ConnectContainer": true, "ContainerIsSystem": true, "CreateVolume": true,
	"CreateVolumeOpts": true, "DeleteVolumeFile": true, "PushImageDigest": true,
	"Exec": true, "ExecShell": true, "ExecStream": true, "ExecTTY": true,
	"HealthStatus": true, "InspectContainer": true, "InspectVolume": true,
	"ListAll": true, "ListByLabel": true, "ListVolumeFiles": true,
	"ListVolumes": true, "LocalDigest": true, "Logs": true,
	"NetworkMemberAddr": true, "PauseContainer": true, "Prune": true,
	"PullImage": true, "PurgeVolume": true, "PushImage": true,
	"ReadVolumeFile": true, "RemoveImage": true, "RemoveVolume": true,
	"RestartContainer": true, "StartContainer": true, "Stats": true,
	"StopContainer": true, "StopRemove": true, "StreamLogs": true,
	"StreamLogsMarked": true, "TarVolume": true, "UnpauseContainer": true,
	"UntarVolume": true, "VerifyTar": true, "VolumeSize": true,
	"WriteVolumeFile": true,
}

// allowed is every call site that may still name one of those methods, with
// the reason. Two kinds live here and nothing else may be added without one:
//
//   - the door itself and the layers under it, which is where the decision is
//     made rather than dodged;
//   - a handful of receivers that share a method name with the list by
//     accident and have nothing to do with docker.
//
// The gate exists because the same check run by hand as a grep let three real
// gaps be written down as "for later" instead of fixed (docs/plans/36-cluster-only.md).
var allowed = map[string]string{
	"internal/stackrd/infra/cluster":  "the door: this is where local-or-agent is decided",
	"internal/stackrd/infra/runtime":  "the socket layer the door calls",
	"internal/stackrd/infra/agent":    "the node's own side of the door, and the client that dials it",
	"internal/stackrd/infra/registry": "a dependency of cluster (cluster -> agent -> registry), so it cannot import the door; its one container is the pre-swarm registry, pinned to the manager by design",
	"internal/cli":                    "the CLI speaks HTTP to the panel; any name match here is its own",
	"internal/proxyrelay":             "the relay has no docker socket at all",
}

// TestNothingOutsideClusterTalksToTheSocket walks the tree and fails on any
// node-scoped method call made on a *runtime.Runtime outside the allowed
// packages.
//
// It works on names, not on a full type check: per package it collects every
// identifier declared as a *runtime.Runtime (struct field, parameter,
// variable) and then flags calls whose receiver ends in one of those names.
// That is enough here and costs no dependency; a same-named local of another
// type would be a false positive, which is an allowed entry away.
func TestNothingOutsideClusterTalksToTheSocket(t *testing.T) {
	root := repoRoot(t)
	var bad []string
	for _, dir := range []string{"internal", "cmd"} {
		byPkg := map[string][]*parsed{}
		walk(t, root, dir, func(p *parsed) {
			byPkg[filepath.ToSlash(filepath.Dir(p.rel))] = append(byPkg[filepath.ToSlash(filepath.Dir(p.rel))], p)
		})
		for pkgDir, files := range byPkg {
			if allowedPkg(pkgDir) != "" {
				continue
			}
			// Runtime() is the sanctioned way for the swarm plumbing to reach
			// the manager's socket, which makes it the obvious back door:
			// c.Runtime().StopRemove(...) is a node call with no node. Seeded
			// here so it is flagged like any other.
			names := map[string]bool{"Runtime": true}
			for _, f := range files {
				runtimeNames(f.file, names)
			}
			if len(names) == 0 {
				continue
			}
			for _, f := range files {
				ast.Inspect(f.file, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || !nodeScoped[sel.Sel.Name] || !names[recvName(sel.X)] {
						return true
					}
					pos := f.fset.Position(sel.Pos())
					bad = append(bad, f.rel+":"+itoa(pos.Line)+" "+recvName(sel.X)+"."+sel.Sel.Name)
					return true
				})
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("%d node-scoped docker calls outside infra/cluster.\n"+
			"Each has to name the node it runs on and go through the cluster door:\n  %s",
			len(bad), strings.Join(bad, "\n  "))
	}
}

// recvName is the last identifier of a receiver expression, so both "rt" and
// "a.rt" answer "rt".
func recvName(x ast.Expr) string {
	switch v := x.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.CallExpr:
		if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
			return sel.Sel.Name
		}
	}
	return ""
}

// runtimeAlias is the local name infra/runtime is imported under in one file,
// or "" when it is not imported at all. main.go calls it stackruntime, which
// a hardcoded "runtime" quietly missed.
func runtimeAlias(f *ast.File) string {
	const path = `"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"`
	for _, im := range f.Imports {
		if im.Path.Value != path {
			continue
		}
		if im.Name != nil {
			return im.Name.Name
		}
		return "runtime"
	}
	return ""
}

// runtimeNames collects every identifier in one file declared as a
// *runtime.Runtime.
func runtimeNames(f *ast.File, out map[string]bool) {
	pkg := runtimeAlias(f)
	if pkg == "" {
		return
	}
	isRuntime := func(e ast.Expr) bool {
		star, ok := e.(*ast.StarExpr)
		if !ok {
			return false
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Runtime" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == pkg
	}
	add := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, fld := range fl.List {
			if !isRuntime(fld.Type) {
				continue
			}
			for _, n := range fld.Names {
				out[n.Name] = true
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.StructType:
			add(v.Fields)
		case *ast.FuncType:
			add(v.Params)
		case *ast.ValueSpec:
			if isRuntime(v.Type) {
				for _, n := range v.Names {
					out[n.Name] = true
				}
			}
		case *ast.AssignStmt:
			// "rt, err := runtime.New(...)": the type is not written down, so
			// the constructor's package is what gives it away.
			if len(v.Rhs) != 1 || len(v.Lhs) == 0 {
				return true
			}
			call, ok := v.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg {
				if n, ok := v.Lhs[0].(*ast.Ident); ok {
					out[n.Name] = true
				}
			}
		}
		return true
	})
}

func allowedPkg(dir string) string {
	for p, why := range allowed {
		if dir == p || strings.HasPrefix(dir, p+"/") {
			return why
		}
	}
	return ""
}

// parsed is one source file with the position table that reads it.
type parsed struct {
	rel  string
	file *ast.File
	fset *token.FileSet
}

// Test files are skipped: a fake in a test is not a call to the socket, and
// mocks legitimately carry these method names.
func walk(t *testing.T, root, sub string, fn func(*parsed)) {
	t.Helper()
	base := filepath.Join(root, sub)
	fset := token.NewFileSet()
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		if strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, "_templ.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, p)
		fn(&parsed{rel: filepath.ToSlash(rel), file: f, fset: fset})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", sub, err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	return dir
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
