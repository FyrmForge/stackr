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

// A files: source whose object id moved between the env's config commit and
// the release's one is a "file changed" row and a redeploy of that tile; the
// same id under another commit is nothing; a folder compares as one id; a
// commit the clone lacks counts as changed.
func TestPlanFilesChangedRedeploys(t *testing.T) {
	w := setup(t)
	repo := map[string]string{"conf/app.yml": "a", "conf/sub/b.yml": "b", "other.txt": "x"}
	ids := map[string]string{} // commit|path -> object id
	mounts := false
	w.f.FileID = func(_ context.Context, _ store.Stack, commit, path string) (string, error) {
		id, ok := ids[commit+"|"+path]
		if !ok {
			return "", errors.New("commit not in the clone")
		}
		return id, nil
	}
	w.f.Config = func(_ context.Context, _ store.Stack, _ string, _ io.Writer) ([]byte, Fetcher, error) {
		file := shopFile
		if mounts { // the first apply runs without: a deploy reads a real clone for files
			file = strings.Replace(shopFile, "      port: 80\n", "      port: 80\n      files: [\"conf/app.yml:/etc/app.yml\", \"conf:/etc/conf\"]\n", 1)
		}
		return []byte(file), treeFetch(repo), nil
	}
	w.fake.Digests = map[string]string{"nginx:2": "sha256:two"}
	set := func(commit, app, folder string) {
		ids[commit+"|conf/app.yml"] = app
		ids[commit+"|conf"] = folder
	}
	set("c1", "A1", "T1")
	r1 := w.release(t, "c1")
	_, err := w.f.Apply(ctx, w.dev.ID, r1.ID, io.Discard, nil)
	must(t, err)
	w.running(w.tileIn(t, w.dev, "api"))
	mounts = true

	plan := func(commit string) *Plan {
		p, err := w.f.Plan(ctx, w.dev.ID, w.release(t, commit).ID, io.Discard)
		must(t, err)
		if p.Blocked() {
			t.Fatalf("blocked: %v", p.Blockers)
		}
		return p
	}
	// rows are the "file changed" notes; the first apply ran without the
	// mounts, so the plans carry unrelated rows too.
	rows := func(p *Plan) (n []string) {
		for _, c := range p.Changes {
			if strings.HasPrefix(c.Note, "file changed: ") {
				n = append(n, c.Note)
			}
		}
		return n
	}
	set("c2", "A1", "T1") // same ids, another commit
	if p := plan("c2"); len(rows(p)) != 0 {
		t.Errorf("same file, other commit: %v", rows(p))
	}
	set("c4", "A1", "T2") // a file inside the mounted folder
	if p := plan("c4"); strings.Join(rows(p), "|") != "file changed: conf" {
		t.Errorf("folder changed: %v", rows(p))
	}
	if p := plan("c5"); len(rows(p)) != 2 { // not in the clone: both rows
		t.Errorf("missing commit: %v", rows(p))
	}
	set("c3", "A2", "T1")
	_, wk, err := w.f.plan(ctx, w.dev.ID, w.release(t, "c3").ID, io.Discard)
	must(t, err)
	if len(wk.redeploy) != 1 || !wk.redeploy["api"] {
		t.Errorf("redeploy = %v", wk.redeploy)
	}
}
