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

// Point 18's third net: no handler behind a mutating route may check a role.
//
// The first two tests prove a route NAMES a verb (routeverb_test.go) and that
// the verb's level is the one the route enforced before (routelevel_test.go).
// Neither notices a handler that ALSO keeps its old check, which is how a
// half-finished migration leaves two authorization paths — the thing point 18
// exists to remove.
//
// So this walks the handler tree, resolves per function the set of gate
// helpers its body can reach (transitively, and only through same-package
// calls), and fails any function serving a mutating route that reaches one.
//
// Reads are deliberately out of scope. 192 GET routes are still authorized by
// these same helpers, and gating them is a separate job with its own capture
// — see 06-points-18-20.md. That is why the helpers still exist at all.

// roleChecks is what is left of the authorization vocabulary: the helpers that
// still answer "may this user", as opposed to loading a row.
//
// It shrank when the reads were gated, because most of these stopped checking
// anything. requireTile, requireBackup, requireEnvWrite, ownedOrg,
// ownedSettingsOrg and the rest became loaders and were renamed or reduced to
// a call through; requireOrgOwner, requireOwnerOf and requireOwnerVerb were
// deleted outright. What remains is the set the KindDeferred routes need,
// because for those the org arrives with the request and the route's gate has
// nothing to resolve before the handler runs.
//
// CanWrite, CanWriteOrg, CanWriteHere and IsOwner are deliberately NOT here
// any more. They survive as rendering flags — mask a secret, hide a button —
// which is what they now do everywhere, and a test that cannot tell a flag
// from a gate reports every page that draws a disabled button. A REFUSAL
// built on one of them is still wrong and still belongs at the route; what
// catches that is the captured-level tables, which say what each route
// enforces, and ReadOnlyGuard, which refuses a viewer's writes site-wide.
var roleChecks = map[string]bool{
	// panel
	"RequireOrgWrite": true, "RequireStackAccess": true, "RequireOrgAccess": true,
	// api
	"requireOrgWrite": true, "requireStackAccess": true, "requireEnvAccess": true,
	"requireOrg": true, "orgMember": true, "orgForCreate": true,
}

// boolGated helpers took a `write bool` whose write half was a role check.
// None are left: requireConfigStack still takes the flag, but what it guards
// now is "this stack has a config repo bound", a 409 about the stack rather
// than about the caller.
var boolGated = map[string]bool{}

type fnInfo struct {
	pkg   string
	calls []string
}

// scanHandlers resolves, per "pkg.Func", the gate helpers its body reaches.
func scanHandlers(t *testing.T, root string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	byKey := map[string]*fnInfo{}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".go") ||
			strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, "_templ.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return nil
		}
		// A selector call whose receiver is an imported package is
		// cross-package: its name may match the vocabulary, but it must not
		// be followed as a local edge. annotate.Delete resolving to the
		// package's own Delete is how a personal route once read as owner.
		imports := map[string]bool{}
		for _, im := range f.Imports {
			name := strings.Trim(im.Path.Value, `"`)
			if i := strings.LastIndex(name, "/"); i >= 0 {
				name = name[i+1:]
			}
			if im.Name != nil {
				name = im.Name.Name
			}
			imports[name] = true
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
				e = &fnInfo{pkg: pkg}
				byKey[key] = e
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				ce, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				local := true
				switch x := ce.Fun.(type) {
				case *ast.SelectorExpr:
					name = x.Sel.Name
					// Local only when the receiver is a bare identifier that
					// is not a package: h.loadStack is this package's, and so
					// is a method on a local variable. h.tiles.Create is not —
					// it is TileService's, and following it resolved four
					// stack-create handlers to this package's own Create and
					// reported them as gated when they are not.
					id, ok := x.X.(*ast.Ident)
					if !ok || imports[id.Name] {
						local = false
					}
				case *ast.Ident:
					name = x.Name
				default:
					return true
				}
				if boolGated[name] {
					for _, a := range ce.Args {
						if id, ok := a.(*ast.Ident); ok && id.Name == "true" {
							e.calls = append(e.calls, "!"+name)
						}
					}
				}
				if !local {
					name = "~" + name
				}
				e.calls = append(e.calls, name)
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
		for _, c := range e.calls {
			switch {
			case strings.HasPrefix(c, "!"):
				got[strings.TrimPrefix(c, "!")+"(write)"] = true
			case strings.HasPrefix(c, "~"):
				if n := strings.TrimPrefix(c, "~"); roleChecks[n] {
					got[n] = true
				}
			default:
				if roleChecks[c] {
					got[c] = true
				}
				for k := range reach(e.pkg+"."+c, seen) {
					got[k] = true
				}
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

// mutatingHandlers reads the handler behind each mutating route out of the
// captured-level table, which records it in the comment on every row.
//
// Echo's Route.Name used to carry the handler's function name, which is what
// an earlier version of this test read. It now carries the VERB — that is the
// declaration point 18 put there — so the route no longer knows its handler
// and the table is where the mapping lives.
func mutatingHandlers(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, line := range strings.Split(capturedLevels, "\n") {
		i := strings.Index(line, "# ")
		if i < 0 {
			continue
		}
		// First field only. A row may carry a note after the handler name —
		// the table's header asks a deliberate level change to say what
		// decided it — and taking the whole rest of the line would turn the
		// name into one that matches no handler, silently dropping it from
		// this scan's coverage.
		fields := strings.Fields(line[i+2:])
		if len(fields) == 0 {
			continue
		}
		if fn := fields[0]; strings.Contains(fn, ".") {
			out[fn] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("no handlers parsed out of capturedLevels")
	}
	return out
}

func TestNoMutatingHandlerChecksARole(t *testing.T) {
	gates := scanHandlers(t, "..")
	known := map[string]bool{}
	knownGates := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(stillBodyGated), "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		name, g, ok := strings.Cut(l, " -> ")
		if !ok {
			t.Fatalf("malformed stillBodyGated row (want \"pkg.Func -> gates\"): %q", l)
		}
		known[strings.TrimSpace(name)] = true
		knownGates[strings.TrimSpace(name)] = strings.TrimSpace(g)
	}

	var offenders, stale, drifted []string
	seen := mutatingHandlers(t)
	for label := range seen {
		g, gated := gates["../"+label]
		row := label + " -> " + strings.Join(g, ", ")
		switch {
		case gated && !known[label]:
			offenders = append(offenders, row)
		case !gated && known[label]:
			stale = append(stale, label)
		case gated && known[label] && knownGates[label] != strings.Join(g, ", "):
			// The SET matters, not just that there is one. A handler that
			// swapped RequireStackAccess (the resource's org) for CanWrite
			// (the active cookie org) still "checks a role" and would slip
			// through an emptiness test — that is the active-vs-resource
			// confusion the captured table keeps a column for.
			drifted = append(drifted, row+"\n     was: "+knownGates[label])
		}
	}
	sort.Strings(offenders)
	sort.Strings(stale)
	if len(offenders) > 0 {
		t.Errorf("%d mutating handler(s) still check a role in the body. The route's\n"+
			"verb is the authorization now; the body check is the second path point 18\n"+
			"removes. Strip it, or add the handler to stillBodyGated with the reason:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("%d handler(s) on stillBodyGated no longer check a role; remove them:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
	sort.Strings(drifted)
	if len(drifted) > 0 {
		t.Errorf("%d handler(s) changed WHICH check they make. A different gate can mean a\n"+
			"different org (active cookie vs the resource's own), which is a tenancy change\n"+
			"even at the same level. Update the row only if the change is deliberate:\n  %s",
			len(drifted), strings.Join(drifted, "\n  "))
	}
	t.Logf("%d mutating handlers, %d still body-gated", len(seen), len(known))
}

// stillBodyGated is what is left, and it is not a worklist any more: these are
// the mutating handlers that MUST keep a check in the body, with the reason.
//
// Everything else was stripped when the reads were gated. Once every route on
// both surfaces names a verb, a gate helper inside a handler is a second
// authorization path saying the same thing — and the helpers themselves became
// loaders (a.tile, a.stack, a.env, a.org, h.settingsOrg, h.ownedOrg) or were
// deleted (requireOrgOwner, requireOwnerOf, requireOwnerVerb).
//
// What cannot be stripped is a route whose org is not in its path:
//
//   - service.KindDeferred: the org arrives in the body, or is what the
//     request asks to resolve, so the route's gate has nothing to look up and
//     runs no check at all. For these the body IS the only check and removing
//     it opens the route — which is exactly what happened once to resolvePath
//     and was caught by reading the five deferred handlers, not by a test.
//   - A move: web/handler/org.MoveStack is gated on the org the stack is
//     LEAVING (KindStack on :id), and the org it moves INTO comes off the
//     submitted form. One half at the route, one half in the body, and the
//     body half is not optional — stripping it let an owner of org A push a
//     stack into org B they only view.
//   - The panel's backup destinations, registered on an adminOnly route with
//     KindNone: the org comes off the submitted form, so destOrg checks write
//     rights in whichever org the request names.
//   - Handlers that share a resolver with one of the above
//     (delete/patchDomainResource share resolveResourceTenancy with the
//     create, deleteDestination shares the visibility filter). The route gate
//     answers them too; the shared check is the same answer twice, not a
//     second opinion.
//
// A new entry here needs one of those reasons. A handler that just kept its
// old gate does not belong on this list, it belongs stripped.
const stillBodyGated = `
api/v1.createDestination -> requireOrgWrite
api/v1.createDomainResource -> orgMember, requireOrg, requireOrgWrite, requireStackAccess
api/v1.createStack -> orgForCreate
api/v1.deleteDestination -> orgMember
api/v1.deleteDomainResource -> orgMember, requireOrg, requireOrgWrite, requireStackAccess
api/v1.patchDomainResource -> orgMember, requireOrg, requireOrgWrite, requireStackAccess
api/v1.resolvePath -> requireEnvAccess
web/handler/org.MoveStack -> RequireOrgWrite
web/handler/project.Create -> RequireOrgWrite
web/handler/settings.CreateDestination -> RequireOrgWrite
web/handler/settings.DeleteDestination -> RequireOrgWrite
`
