package servicetest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// Git stands in for GitHub: https://github.com/<owner>/<name> clones from
// a bare repo under a temp root, with no connector and no token. Pass
// Option to NewWith; write commits with Commit.
type Git struct {
	root string
}

func NewGit(t *testing.T) *Git {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return &Git{root: t.TempDir()}
}

// Option clones through the root: file:// allowed, github.com rewritten.
func (g *Git) Option() service.Option {
	return service.WithGit(
		"GIT_ALLOW_PROTOCOL=https:ssh:file",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url.file://"+g.root+"/.insteadOf",
		"GIT_CONFIG_VALUE_0=https://github.com/",
	)
}

// Commit writes files into repo ("acme/shop") on branch, pushes, and
// returns the commit sha. The bare repo is made on first use, its default
// branch the first one committed to.
// ponytail: commits stack on the repo's last commit whatever the branch;
// a second branch forks from there, enough for one-branch tests.
func (g *Git) Commit(t *testing.T, repo, branch string, files map[string]string) string {
	t.Helper()
	bare := filepath.Join(g.root, repo) // the https://github.com/<repo> SetConfigRepo stores
	work := filepath.Join(g.root, "work", repo)
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{
			"-c", "user.name=t",
			"-c", "user.email=t@t",
			"-c", "commit.gpgsign=false",
		}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if _, err := os.Stat(bare); err != nil {
		must(t, os.MkdirAll(work, 0o750))
		git(g.root, "init", "--bare", "--initial-branch="+branch, bare)
		git(work, "init", "--initial-branch="+branch)
		git(work, "remote", "add", "origin", bare)
	}
	for name, body := range files {
		p := filepath.Join(work, name)
		must(t, os.MkdirAll(filepath.Dir(p), 0o750))
		must(t, os.WriteFile(p, []byte(body), 0o600))
	}
	git(work, "add", "-A")
	git(work, "commit", "--allow-empty", "-m", "test")
	git(work, "push", "origin", "HEAD:refs/heads/"+branch)
	return git(work, "rev-parse", "HEAD")
}
