package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// testGit is servicetest.Git for this package (servicetest imports service):
// https://github.com/<repo> clones from a bare repo under root.
func testGit(t *testing.T) (root string, opt Option) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root = t.TempDir()
	return root, WithGit("GIT_ALLOW_PROTOCOL=https:ssh:file", "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url.file://"+root+"/.insteadOf", "GIT_CONFIG_VALUE_0=https://github.com/")
}

// gitCommit writes files into a fresh repo under root on main, pushes it to
// root/<repo> and returns the commit.
func gitCommit(t *testing.T, root, repo string, files map[string]string) string {
	t.Helper()
	work := filepath.Join(root, "work", repo)
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	must(t, os.MkdirAll(work, 0o750))
	git(root, "init", "--bare", "--initial-branch=main", filepath.Join(root, repo))
	git(work, "init", "--initial-branch=main")
	git(work, "remote", "add", "origin", filepath.Join(root, repo))
	for name, body := range files {
		p := filepath.Join(work, name)
		must(t, os.MkdirAll(filepath.Dir(p), 0o750))
		must(t, os.WriteFile(p, []byte(body), 0o600))
	}
	git(work, "add", "-A")
	git(work, "commit", "-m", "test")
	git(work, "push", "origin", "HEAD:refs/heads/main")
	return git(work, "rev-parse", "HEAD")
}

// Files ride a promote env to env: dev (on the controller) promotes its
// release into edge (on worker w1), and edge's tile gets the same files from
// the stack's config repo, written into a volume on w1 and mounted read-only
// where the stack file says.
func TestPromoteCarriesFilesToAnotherNode(t *testing.T) {
	ctx := context.Background()
	root, gitOpt := testGit(t)
	n := newTwoNodes(t, gitOpt)
	const conf = "server { listen 80; }\n"
	sha := gitCommit(t, root, "acme/shop", map[string]string{
		"conf/app.conf": conf,
		"stackr-compose.yml": "version: 1\nstack: shop\nladder: [dev, edge]\nhead: main\nbase:\n  tiles:\n    web:\n" +
			"      image: nginx:1\n      port: 80\n      files: [\"conf/app.conf:/etc/nginx/conf.d/app.conf\"]\n",
	})
	st, err := n.orch.SetConfigRepo(ctx, n.stack, "", "acme/shop", "main", "")
	must(t, err)
	// what each node answers when its tile starts
	n.ctlFake.RunID, n.wFake.RunID = "c1", "w1c"
	n.ctlFake.Details = map[string]docker.Detail{"c1": {Running: true, Networks: map[string]string{n.dev.Network: "10.208.0.2"}}}
	n.wFake.Details = map[string]docker.Detail{"w1c": {Running: true, Networks: map[string]string{n.edge.Network: "10.208.1.2"}}}
	rel, err := n.orch.releases.Create(ctx, n.stack, "test", []release.Pin{{Slug: release.ConfigSlug, Repo: st.ConfigRepo, CommitSHA: sha}})
	must(t, err)
	promote := func(envID, relID string) {
		t.Helper()
		j, err := n.orch.Promote(ctx, pmAdmin(), envID, relID)
		must(t, err)
		if j = n.wait(t, j); j.State != job.Done {
			t.Fatalf("promote into %s = %s %q", envID, j.State, j.Error)
		}
	}
	// files is what the env's web tile got on fake: its mount, and the
	// content of the volume it mounts.
	files := func(fake *dockerfake.Fake, envID string) (vol, got string) {
		t.Helper()
		tl, err := n.orch.tiles.GetBySlug(ctx, envID, "web")
		must(t, err)
		var spec *struct{ vol, sub string }
		for _, s := range fake.Specs {
			for _, m := range s.Mounts {
				if m.Target == "/etc/nginx/conf.d/app.conf" && m.ReadOnly && strings.HasPrefix(m.Volume, "stackr-files-"+tl.ID[:8]) {
					spec = &struct{ vol, sub string }{m.Volume, m.Subpath}
				}
			}
		}
		if spec == nil {
			t.Fatalf("env %s: no read-only files mount in %d specs", envID, len(fake.Specs))
		}
		var labelled bool
		for _, v := range fake.Created {
			labelled = labelled || v.Name == spec.vol && v.Labels[tile.LabelFiles] == tl.ID
		}
		if !labelled {
			t.Errorf("env %s: volume %s not made on its node", envID, spec.vol)
		}
		return spec.vol, member(t, fake.Untarred, spec.sub)
	}

	promote(n.dev.ID, rel.ID)
	devVol, got := files(n.ctlFake, n.dev.ID)
	if got != conf {
		t.Errorf("dev's file = %q", got)
	}
	dev, err := n.st.Environments.Get(ctx, n.dev.ID)
	must(t, err)
	if dev.ReleaseID == nil {
		t.Fatal("dev has no release after its promote")
	}

	promote(n.edge.ID, *dev.ReleaseID)
	edgeVol, got := files(n.wFake, n.edge.ID)
	if got != conf {
		t.Errorf("edge's file = %q", got)
	}
	if edgeVol == devVol {
		t.Error("edge mounts dev's volume")
	}
	for _, v := range n.ctlFake.Created {
		if v.Name == edgeVol {
			t.Error("edge's files were written on the controller")
		}
	}
}

// member is the body of name in a gzipped tar.
func member(t *testing.T, tgz []byte, name string) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	must(t, err)
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			t.Fatalf("no %s in the archive", name)
		}
		must(t, err)
		if h.Name == name {
			b, err := io.ReadAll(tr)
			must(t, err)
			return string(b)
		}
	}
}

// A tile the stack file its env took does not declare is outside the file
// (the next promote removes it); a declared one is not, nor is any tile of
// a stack with no file.
func TestTileOutsideStackFile(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	web, extra := w.tile(t, "web", false), w.tile(t, "extra", false)
	if w.orch.TileOutsideStackFile(ctx, extra) {
		t.Fatal("no stack file: extra is outside it")
	}
	// the clone a promote leaves under the data dir
	sha := gitCommit(t, filepath.Join(w.orch.cfg.DataDir, "repos"), "config-"+w.stack, map[string]string{
		"stackr-compose.yml": "version: 1\nstack: shop\nladder: [dev]\nhead: main\nbase:\n  tiles:\n    web:\n      image: nginx:1\n      port: 80\n",
	})
	st, err := w.st.Stacks.Get(ctx, w.stack)
	must(t, err)
	st.ConfigRepo, st.ConfigBranch = "https://github.com/acme/shop", "main"
	must(t, w.st.Stacks.Update(ctx, st))
	rel, err := w.orch.releases.Create(ctx, w.stack, "test", []release.Pin{{Slug: release.ConfigSlug, Repo: st.ConfigRepo, CommitSHA: sha}})
	must(t, err)
	e, err := w.orch.envs.Get(ctx, w.env)
	must(t, err)
	_, err = w.orch.envs.SetRelease(ctx, e, rel.ID)
	must(t, err)
	if w.orch.TileOutsideStackFile(ctx, web) {
		t.Error("web is declared, yet outside the file")
	}
	if !w.orch.TileOutsideStackFile(ctx, extra) {
		t.Error("extra is not declared, yet not outside the file")
	}
}
