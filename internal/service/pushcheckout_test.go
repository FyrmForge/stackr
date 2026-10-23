package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
)

// A config push that adds a tile built from git builds it before the tile
// has a row. Its checkout is keyed on the stack and slug, never on the empty
// id: that once made the checkout the repos folder itself, and the clone
// wiped every cached checkout (stack, org and server config) in it.
func TestPushNewGitTileKeepsOtherCheckouts(t *testing.T) {
	ctx := context.Background()
	root, gitOpt := testGit(t)
	n := newTwoNodes(t, gitOpt)
	n.orch.cfg.FakeHost = t.TempDir() // no cluster registry to push the image to
	sha := gitCommit(t, root, "acme/shop", map[string]string{
		"Dockerfile": "FROM scratch\n",
		"stackr-compose.yml": "version: 1\nstack: shop\nladder: [dev, edge]\nhead: main\nbase:\n  tiles:\n    api:\n" +
			"      type: service\n      build: {context: .}\n      port: 80\n",
	})
	if _, err := n.orch.SetConfigRepo(ctx, n.stack, "", "acme/shop", "main", ""); err != nil {
		t.Fatal(err)
	}
	repos := filepath.Join(n.dir, "repos")
	var planted []string
	for _, d := range []string{"orgconfig-o1", "serverconfig"} {
		p := filepath.Join(repos, d, ".git", "HEAD")
		must(t, os.MkdirAll(filepath.Dir(p), 0o750))
		must(t, os.WriteFile(p, nil, 0o600))
		planted = append(planted, p)
	}

	var log strings.Builder
	rel, _, err := n.orch.promote.Push(ctx, n.stack, promote.Event{
		Repo: "https://github.com/acme/shop", Branch: "main", Commit: sha,
	}, &log)
	if err != nil {
		t.Fatalf("push: %v\n%s", err, log.String())
	}
	if rel.ID == "" {
		t.Fatalf("push built nothing\n%s", log.String())
	}
	for _, p := range append(planted, filepath.Join(repos, "config-"+n.stack, ".git")) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("checkout gone after the push: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(repos, ".git")); err == nil {
		t.Error("the tile was cloned into the repos folder itself")
	}
	tiles, _ := filepath.Glob(filepath.Join(repos, "tile-"+n.stack+"-api-*", ".git"))
	if len(tiles) != 1 {
		t.Errorf("tile checkouts = %v, want one keyed on the stack and slug", tiles)
	}
}
