package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
)

// The boot extension check reads the operated, tiles and settings leaves:
// New starts it only after all three are assigned, and the node watcher
// (started earlier in New) never does.
func TestBootPGExtCheckStartsAfterTheWiring(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "orchestrator.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var start token.Pos
	wired := map[string]token.Pos{}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); !ok || fn.Name.Name != "New" {
			continue
		} else {
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					for _, l := range n.Lhs {
						if s, ok := l.(*ast.SelectorExpr); ok {
							wired[s.Sel.Name] = n.Pos()
						}
					}
				case *ast.SelectorExpr:
					if n.Sel.Name == "bootPGExtCheck" {
						start = n.Pos()
					}
				}
				return true
			})
		}
	}
	if start == token.NoPos {
		t.Fatal("New never starts bootPGExtCheck")
	}
	for _, leaf := range []string{"operated", "tiles", "settings"} {
		if p, ok := wired[leaf]; !ok || p > start {
			t.Errorf("bootPGExtCheck starts before orch.%s is wired", leaf)
		}
	}
	w, err := parser.ParseFile(token.NewFileSet(), "nodeclients.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(w, func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok && s.Sel.Name == "bootPGExtCheck" {
			t.Error("watchNodeClients starts bootPGExtCheck, before New has wired the leaves")
		}
		return true
	})
}

// A run where an instance's check failed does not stamp the run time, so
// the next boot checks again; a clean run stamps it.
func TestPGExtCheckStampsOnlyACleanRun(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	r.countResolves("0.8.7-1")
	r.markBuilt(t, r.pg.ID, "ref1", "0.8.6-1")
	stamped := func() bool { _, ok, _ := r.orch.store.Settings.Get(r.ctx, pgextCheckedKey); return ok }
	must(t, r.orch.store.Settings.Delete(r.ctx, pgextCheckedKey))
	r.orch.pgFns.Parse = func(string) (mflow.Resolved, error) { return mflow.Resolved{}, errTest("apt is down") }
	r.orch.checkPGExtUpdates(r.ctx)
	if stamped() {
		t.Error("a run whose only check failed was stamped")
	}
	r.orch.pgFns.Parse = func(string) (mflow.Resolved, error) {
		return mflow.Resolved{Versions: map[string]string{}}, nil
	}
	r.orch.checkPGExtUpdates(r.ctx)
	if !stamped() {
		t.Error("a clean run was not stamped")
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
