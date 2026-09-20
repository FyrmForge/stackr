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
// The walk counts two spellings of the same call: `h.store.GetX(…)` on a
// field, and `store.GetX(…)` on a repo.Store the function was handed. It used
// to count only the first, and three functions in handler/settings were
// already through that hole — a helper that takes the store reads it as a bare
// identifier, which is indistinguishable from a call to this package's own
// code unless the parameter types are read.
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
			// Parameters and receivers declared as repo.Store. A helper that
			// TAKES the store reads it as a bare identifier — `store.GetOrg(…)`
			// — which looks exactly like a call to a function in this package,
			// so without this the read is filed as a local edge and vanishes.
			// Two of them were already through that hole.
			bare := map[string]bool{}
			for _, fl := range params(fd) {
				if !isRepoStore(fl.Type) {
					continue
				}
				for _, nm := range fl.Names {
					bare[nm.Name] = true
				}
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
				// deeper (h.tiles.Create) belongs to another package. A bare
				// identifier that IS the store is the banned call, not an edge.
				if id, ok := sel.X.(*ast.Ident); ok {
					if bare[id.Name] {
						e.store = append(e.store, sel.Sel.Name)
					} else {
						e.calls = append(e.calls, sel.Sel.Name)
					}
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

// stillStoreReading is what is LEFT of point 19: handler functions whose body
// still reaches the store, directly or through a helper in the same package.
//
// It started at 525 functions, seeded from the scan rather than typed, and it
// only shrinks. One is left, and it is not work:
//
// The health endpoint pings the store to answer "is the database there". That
// is a liveness check on the dependency itself, not a read of anything a
// service could own — a StoreHealthService would be the forwarder this whole
// point exists to avoid.
//
// What this test does NOT measure: a handler PASSING h.store to a lower layer.
// Several still do (stackconf.Planner, envnet, placement, sharelink, the
// settings cascade, audit.Record), because those helpers take a repo.Store and
// converting them is a separate job from moving the reads. The scan counts
// calls, not passes, and saying so here is cheaper than someone later reading
// a green test as "no handler has a store".

const stillStoreReading = `
handlers/api/handler/health.Health -> Health
`

// params is a function's receiver plus its parameters, flattened.
func params(fd *ast.FuncDecl) []*ast.Field {
	var out []*ast.Field
	if fd.Recv != nil {
		out = append(out, fd.Recv.List...)
	}
	if fd.Type.Params != nil {
		out = append(out, fd.Type.Params.List...)
	}
	return out
}

// isRepoStore reports whether a type expression is repo.Store. The import is
// always named repo in these packages; an alias would read as a local type and
// slip through, which is a smaller hole than the one this closes.
func isRepoStore(x ast.Expr) bool {
	sel, ok := x.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Store" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "repo"
}
