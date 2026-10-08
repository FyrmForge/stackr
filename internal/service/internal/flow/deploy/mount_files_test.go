package deploy_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// filesWorld is setup with a config clone at <DataDir>/repos/config-<stack>
// holding conf/app.yml, conf/sub/b.yml and a release that pins its commit.
func filesWorld(t *testing.T) (*world, string) {
	w := setup(t)
	w.f.DataDir = t.TempDir()
	dir := filepath.Join(w.f.DataDir, "repos", "config-"+w.tile.StackID)
	for name, body := range map[string]string{
		"conf/app.yml":   "key: ${{ params.app.key }}\n",
		"conf/sub/b.yml": "b ${{ not.expanded }}\n",
	} {
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750))
		must(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--initial-branch=main")
	git("add", "-A")
	git("commit", "-m", "c")
	commit := git("rev-parse", "HEAD")

	st, err := w.f.Stacks.Get(ctx, w.tile.StackID)
	must(t, err)
	st.ConfigRepo = "https://github.com/acme/shop"
	must(t, w.st.Stacks.Update(ctx, st))
	r, err := w.f.Releases.Derive(ctx, st.ID, "", "test", release.Pin{Slug: release.ConfigSlug, CommitSHA: commit})
	must(t, err)
	w.env, err = w.f.Envs.SetRelease(ctx, w.env, r.ID)
	must(t, err)
	must(t, w.f.Params.Merge(ctx, params.Scope{Kind: "env", ID: w.env.ID}, []params.Entry{
		{Collection: "app", Name: "key", Kind: params.Secret, Value: "s3cret"},
	}))
	// The listed replica mounts whatever folder the last run bound, so the
	// post-deploy prune keeps it.
	tiles := tile.New(w.st.Tiles, mountFake{w.fake}, vipStub{})
	tiles.Gate = w.f.Tiles.Gate
	w.f.Tiles = tiles
	w.fake.RunIDs = nil
	w.fake.RunID = "new"
	return w, commit
}

// mountFake makes the old replica mount every bind a Run was given.
type mountFake struct{ *dockerfake.Fake }

func (m mountFake) mount(s docker.ContainerSpec) {
	d := m.Details["old"]
	d.Mounts = nil
	for _, v := range s.Volumes {
		d.Mounts = append(d.Mounts, strings.SplitN(v, ":", 2)[0]+" -> "+strings.Split(v, ":")[1])
	}
	m.Details["old"] = d
}

func (m mountFake) Run(ctx context.Context, s docker.ContainerSpec) (string, error) {
	m.mount(s)
	return m.Fake.Run(ctx, s)
}

// Create is the stop-first path's Run.
func (m mountFake) Create(ctx context.Context, s docker.ContainerSpec) (string, error) {
	m.mount(s)
	return m.Fake.Create(ctx, s)
}

func TestFilesLandReadOnly(t *testing.T) {
	w, commit := filesWorld(t)
	w.tile.Files = "conf/app.yml:/etc/app.yml:template\nconf:/etc/conf"
	_, err := w.f.Run(ctx, w.tile, "nginx@sha256:aa", io.Discard, nil)
	must(t, err)
	base := filepath.Dir(filepath.Dir(bindSrc(w, 0)))
	if !strings.HasPrefix(filepath.Base(base), commit+"-") {
		t.Fatalf("folder %s is not keyed on the commit", base)
	}
	want := []string{
		filepath.Join(base, "0", "app.yml") + ":/etc/app.yml:ro",
		filepath.Join(base, "1") + ":/etc/conf:ro",
	}
	if got := w.fake.Specs[0].Volumes; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("binds = %v, want %v", got, want)
	}
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(base, p))
		must(t, err)
		return string(b)
	}
	if got := read("0/app.yml"); got != "key: s3cret\n" {
		t.Errorf("template = %q", got)
	}
	// Folder lines copy byte for byte unless flagged.
	if got := read("1/sub/b.yml"); got != "b ${{ not.expanded }}\n" {
		t.Errorf("plain copy = %q", got)
	}
	if got := read("1/app.yml"); got != "key: ${{ params.app.key }}\n" {
		t.Errorf("plain copy = %q", got)
	}
}

func TestFilesPrune(t *testing.T) {
	w, _ := filesWorld(t)
	w.tile.Files = "conf/app.yml:/etc/app.yml"
	_, err := w.f.Run(ctx, w.tile, "nginx@sha256:aa", io.Discard, nil)
	must(t, err)
	root := filepath.Join(w.f.DataDir, "files", w.tile.ID)
	stale := filepath.Join(root, "oldcommit", "0")
	must(t, os.MkdirAll(stale, 0o750))
	live := filepath.Dir(filepath.Dir(bindSrc(w, 0)))
	must(t, deploy.PruneFiles(w.f, w.tile, []string{filepath.Join(live, "0", "app.yml") + " -> /etc/app.yml"}))
	if _, err := os.Stat(stale); err == nil {
		t.Error("the unused commit folder survived")
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("the mounted commit folder was pruned: %v", err)
	}
}

func TestFilesNeedTheConfigRepo(t *testing.T) {
	w := setup(t) // a hand-made stack: no config repo, no config pin
	w.tile.Files = "conf/app.yml:/etc/app.yml"
	_, err := w.f.Run(ctx, w.tile, "nginx@sha256:aa", io.Discard, nil)
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "files: needs the stack's config repo") {
		t.Errorf("err = %v", err)
	}
}

// bindSrc is the host path of the n-th bind of the last deployed spec.
func bindSrc(w *world, n int) string {
	v := w.fake.Specs[len(w.fake.Specs)-1].Volumes[n]
	return strings.SplitN(v, ":", 2)[0]
}

func mustRun(t *testing.T, w *world) {
	t.Helper()
	_, err := w.f.Run(ctx, w.tile, "nginx@sha256:aa", io.Discard, nil)
	must(t, err)
}

func readBind(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	must(t, err)
	return string(b)
}

// A drawer edit of files: at the same commit remounts the new content, adds a
// new folder line and renders a line that became :template.
func TestFilesEditAtSameCommit(t *testing.T) {
	w, _ := filesWorld(t)
	w.tile.Files = "conf/app.yml:/etc/app.yml"
	mustRun(t, w)
	if got := readBind(t, bindSrc(w, 0)); got != "key: ${{ params.app.key }}\n" {
		t.Fatalf("first = %q", got)
	}
	first := bindSrc(w, 0)

	w.tile.Files = "conf/app.yml:/etc/app.yml:template\nconf:/etc/conf"
	mustRun(t, w)
	if bindSrc(w, 0) == first {
		t.Fatal("the folder was reused after the files: lines changed")
	}
	if got := readBind(t, bindSrc(w, 0)); got != "key: s3cret\n" {
		t.Errorf("template line stayed raw: %q", got)
	}
	if got := readBind(t, filepath.Join(bindSrc(w, 1), "sub", "b.yml")); got != "b ${{ not.expanded }}\n" {
		t.Errorf("new folder line = %q", got)
	}

	// Same lines, same params: the folder is reused, not rewritten.
	again := bindSrc(w, 0)
	mustRun(t, w)
	if bindSrc(w, 0) != again {
		t.Error("an unchanged tile got a new folder")
	}
}

// A param edit re-renders a template; the path shows the params version, not
// a value.
func TestFilesParamEditRemounts(t *testing.T) {
	w, _ := filesWorld(t)
	w.tile.Files = "conf/app.yml:/etc/app.yml:template"
	mustRun(t, w)
	first := bindSrc(w, 0)
	time.Sleep(10 * time.Millisecond)
	must(t, w.f.Params.Merge(ctx, params.Scope{Kind: "env", ID: w.env.ID}, []params.Entry{
		{Collection: "app", Name: "key", Kind: params.Secret, Value: "n3w"},
	}))
	mustRun(t, w)
	if bindSrc(w, 0) == first {
		t.Fatal("the folder was reused after a param edit")
	}
	if got := readBind(t, bindSrc(w, 0)); got != "key: n3w\n" {
		t.Errorf("template = %q", got)
	}
	if strings.Contains(bindSrc(w, 0), "n3w") || strings.Contains(bindSrc(w, 0), "s3cret") {
		t.Error("a secret value is in the path")
	}
}

func TestFilesMissingCommit(t *testing.T) {
	w, _ := filesWorld(t)
	st, err := w.f.Stacks.Get(ctx, w.tile.StackID)
	must(t, err)
	r, err := w.f.Releases.Derive(ctx, st.ID, "", "test", release.Pin{Slug: release.ConfigSlug, CommitSHA: strings.Repeat("a", 40)})
	must(t, err)
	w.env, err = w.f.Envs.SetRelease(ctx, w.env, r.ID)
	must(t, err)
	w.tile.Files = "conf/app.yml:/etc/app.yml"
	_, err = w.f.Run(ctx, w.tile, "nginx@sha256:aa", io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "the config repo clone lacks "+strings.Repeat("a", 12)) {
		t.Errorf("err = %v", err)
	}
}

func TestFilesRefuseEscapingEntry(t *testing.T) {
	for _, rel := range []string{"../x", "a/../../x", "/abs"} {
		if _, err := deploy.SlotPath("/t", "0", rel); err == nil {
			t.Errorf("%q accepted", rel)
		}
	}
	if _, err := deploy.SlotPath("/t", "0", "sub/b.yml"); err != nil {
		t.Error(err)
	}
}
