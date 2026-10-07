package promote

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A files: line must name a file or folder of the config repo at the release's
// commit; a missing path blocks the plan, naming tile, path and commit.
func TestPlanFilesBlocksAMissingPath(t *testing.T) {
	w := setup(t)
	repo := map[string]string{"conf/app.yml": "a", "conf/sub/b.yml": "b"}
	w.f.Config = func(_ context.Context, _ store.Stack, _ string, _ io.Writer) ([]byte, Fetcher, error) {
		file := strings.Replace(shopFile, "      port: 80\n", "      port: 80\n      files: [\"conf/app.yml:/etc/app.yml:template\", \"conf:/etc/conf\"]\n", 1)
		return []byte(file), treeFetch(repo), nil
	}
	r := w.release(t, "c1")
	p, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	if p.Blocked() {
		t.Fatalf("file and folder present, blockers = %v", p.Blockers)
	}

	delete(repo, "conf/app.yml")
	delete(repo, "conf/sub/b.yml")
	p, err = w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	got := strings.Join(p.Blockers, "|")
	if !p.Blocked() || !strings.Contains(got, "tile api: files: conf/app.yml is not in the repo at c1") ||
		!strings.Contains(got, "files: conf is not in the repo") {
		t.Errorf("blockers = %v", p.Blockers)
	}
}

// treeFetch is a Fetcher over a path -> content map; a trailing slash lists
// the files below it, NUL-joined, as Orchestrator.stackFile does.
func treeFetch(repo map[string]string) Fetcher {
	return func(p string) ([]byte, error) {
		if dir, ok := strings.CutSuffix(p, "/"); ok {
			var names []string
			for n := range repo {
				if strings.HasPrefix(n, dir+"/") {
					names = append(names, n)
				}
			}
			sort.Strings(names)
			return []byte(strings.Join(names, "\x00")), nil
		}
		b, ok := repo[p]
		if !ok {
			return nil, errors.New("not found")
		}
		return []byte(b), nil
	}
}
