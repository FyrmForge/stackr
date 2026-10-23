package connectors_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	tree = "github.com/FyrmForge/stackr/internal/service/internal/connectors"
	all  = tree + "/all"
)

// Nothing outside the connectors tree imports a kind package: features ask
// for a capability, and the kinds are named in connectors/all alone, which
// only the orchestrator (internal/service) imports.
func TestKindImports(t *testing.T) {
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "internal", "service", "internal", "connectors") + string(filepath.Separator)
	seen := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasPrefix(path, inside) {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		seen++
		rel, _ := filepath.Rel(root, path)
		for _, im := range f.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			switch {
			case p == all && filepath.Dir(rel) != filepath.Join("internal", "service"):
				t.Errorf("%s imports %s: only the orchestrator registers the kinds", rel, p)
			case p != all && strings.HasPrefix(p, tree+"/"):
				t.Errorf("%s imports kind package %s: ask for a capability instead", rel, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 100 {
		t.Fatalf("walked %d files from %s: the root is wrong", seen, root)
	}
	// the kinds the walk keeps everyone else away from are the registered
	// ones, GitHub's and Namecheap's among them
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(inside, "all", "all.go"), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		kinds[p] = true
	}
	for _, k := range []string{"github", "namecheap"} {
		if !kinds[tree+"/"+k] {
			t.Errorf("connectors/all does not register %s", k)
		}
	}
}
