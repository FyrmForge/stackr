package deploy_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	"github.com/FyrmForge/stackr/internal/service/internal/store"
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

// mountFake is a node that keeps what it is given: CreateVolume lists the
// volume, UntarVolume stores the archive and TarVolume hands it back, which is
// all the files volume's done check reads.
type mountFake struct{ *dockerfake.Fake }

func (m mountFake) CreateVolume(ctx context.Context, name, driver string, opts, labels map[string]string) error {
	m.Volumes = append(m.Volumes, docker.VolumeInfo{Name: name, Labels: labels})
	return m.Fake.CreateVolume(ctx, name, driver, opts, labels)
}

func (m mountFake) TarVolume(ctx context.Context, name string, w io.Writer, live bool) error {
	m.TarOut = m.Untarred
	return m.Fake.TarVolume(ctx, name, w, live)
}

func (m mountFake) RemoveVolume(ctx context.Context, name string) error {
	m.Volumes = slices.DeleteFunc(m.Volumes, func(v docker.VolumeInfo) bool { return v.Name == name })
	return m.Fake.RemoveVolume(ctx, name)
}

// archive is the members of the last archive a volume was written from.
func archive(t *testing.T, w *world) (names []string, body map[string]string) {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(w.fake.Untarred))
	must(t, err)
	body = map[string]string{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names, body
		}
		must(t, err)
		b, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		body[h.Name] = string(b)
	}
}

func count(w *world, method string) (n int) {
	for _, c := range w.fake.Calls() {
		if c.Method == method {
			n++
		}
	}
	return n
}

func TestFilesLandReadOnly(t *testing.T) {
	w, _ := filesWorld(t)
	w.tile.Files = "conf/app.yml:/etc/app.yml:template\nconf:/etc/conf"
	old := filepath.Join(w.f.DataDir, "files", w.tile.ID, "oldcommit", "0")
	must(t, os.MkdirAll(old, 0o750))
	mustRun(t, w)

	s := w.fake.Specs[0]
	if len(s.Mounts) != 2 {
		t.Fatalf("mounts = %+v", s.Mounts)
	}
	vol := s.Mounts[0].Volume
	if !strings.HasPrefix(vol, "stackr-files-"+w.tile.ID[:8]+"-") {
		t.Errorf("volume = %s", vol)
	}
	want := []docker.Mount{
		{Volume: vol, Subpath: "0/app.yml", Target: "/etc/app.yml", ReadOnly: true},
		{Volume: vol, Subpath: "1", Target: "/etc/conf", ReadOnly: true},
	}
	if !slices.Equal(s.Mounts, want) {
		t.Errorf("mounts = %+v, want %+v", s.Mounts, want)
	}
	for _, v := range s.Volumes {
		if strings.Contains(v, "/files/") {
			t.Errorf("a host path is still bound: %s", v)
		}
	}
	names, body := archive(t, w)
	if last := names[len(names)-1]; last != tile.FilesDone {
		t.Errorf("last member = %s", last)
	}
	if got := body["0/app.yml"]; got != "key: s3cret\n" {
		t.Errorf("template = %q", got)
	}
	// Folder lines copy byte for byte unless flagged.
	if got := body["1/sub/b.yml"]; got != "b ${{ not.expanded }}\n" {
		t.Errorf("plain copy = %q", got)
	}
	if got := body["1/app.yml"]; got != "key: ${{ params.app.key }}\n" {
		t.Errorf("plain copy = %q", got)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("the pre-volume folder survived")
	}
	for _, c := range w.fake.Calls() {
		if strings.Contains(c.String(), "s3cret") {
			t.Errorf("a secret reached a Docker call: %s", c.Method)
		}
	}
}

// The same key finds its volume written and writes nothing; an edit of the
// lines makes a new volume and drops the old one.
func TestFilesWrittenOnce(t *testing.T) {
	w, _ := filesWorld(t)
	w.tile.Files = "conf/app.yml:/etc/app.yml"
	mustRun(t, w)
	first := w.fake.Specs[0].Mounts[0].Volume
	mustRun(t, w)
	if n := count(w, "UntarVolume"); n != 1 {
		t.Errorf("untar calls = %d, want 1", n)
	}
	if got := w.fake.Specs[1].Mounts[0].Volume; got != first {
		t.Errorf("volume changed: %s", got)
	}

	w.tile.Files = "conf/app.yml:/etc/app.yml:template\nconf:/etc/conf"
	mustRun(t, w)
	if got := w.fake.Specs[2].Mounts[0].Volume; got == first {
		t.Fatal("the volume was reused after the files: lines changed")
	}
	if !slices.ContainsFunc(w.fake.Calls(), func(c dockerfake.Call) bool {
		return c.Method == "RemoveVolume" && c.Args[0] == first
	}) {
		t.Error("the old volume was not pruned")
	}
}

// A volume cut short (no done marker) is written again.
func TestFilesRewriteUnfinishedVolume(t *testing.T) {
	w, _ := filesWorld(t)
	w.tile.Files = "conf/app.yml:/etc/app.yml"
	mustRun(t, w)
	w.fake.Untarred = nil // the volume holds no marker
	mustRun(t, w)
	if n := count(w, "UntarVolume"); n != 2 {
		t.Errorf("untar calls = %d, want 2", n)
	}
}

// A param edit re-renders a template into a new volume; the name shows
// neither the value nor the params version.
func TestFilesParamEditRemounts(t *testing.T) {
	w, _ := filesWorld(t)
	w.tile.Files = "conf/app.yml:/etc/app.yml:template"
	mustRun(t, w)
	first := w.fake.Specs[0].Mounts[0].Volume
	time.Sleep(10 * time.Millisecond)
	must(t, w.f.Params.Merge(ctx, params.Scope{Kind: "env", ID: w.env.ID}, []params.Entry{
		{Collection: "app", Name: "key", Kind: params.Secret, Value: "n3w-key"},
	}))
	mustRun(t, w)
	second := w.fake.Specs[1].Mounts[0].Volume
	if second == first {
		t.Fatal("the volume was reused after a param edit")
	}
	if _, body := archive(t, w); body["0/app.yml"] != "key: n3w-key\n" {
		t.Errorf("template = %q", body["0/app.yml"])
	}
	if strings.Contains(second, "n3w-key") {
		t.Errorf("a secret value is in the volume name %s", second)
	}
}

// A hook that refuses stops the deploy before anything is pulled or started.
func TestPreflightRefuses(t *testing.T) {
	w := setup(t)
	var got string
	w.f.Preflight = func(_ context.Context, e store.Environment, _ io.Writer) error {
		got = e.ID
		return errs.Conflictf("node is full")
	}
	_, err := w.f.Run(ctx, w.tile, "nginx@sha256:aa", io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "node is full") || got != w.env.ID {
		t.Errorf("err = %v, env = %q", err, got)
	}
	if len(w.fake.Specs) != 0 {
		t.Error("a container started after the refusal")
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

func mustRun(t *testing.T, w *world) {
	t.Helper()
	_, err := w.f.Run(ctx, w.tile, "nginx@sha256:aa", io.Discard, nil)
	must(t, err)
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
		if _, err := deploy.SlotPath("0", rel); err == nil {
			t.Errorf("%q accepted", rel)
		}
	}
	if _, err := deploy.SlotPath("0", "sub/b.yml"); err != nil {
		t.Error(err)
	}
}
