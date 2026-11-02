// Package git clones and checks out source repositories. It shells out to the
// git binary. No store handle, no Docker handle, no decisions about a deploy.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// allowProtocol stops git from following a redirect or a submodule into a
// local or exotic transport. The URL allowlist at the boundary (leaf/tile)
// refuses those in the URL itself; this covers what git resolves on its own.
// A var only so tests can clone a local bare repo.
var allowProtocol = "https:ssh"

// Repo is one working clone on disk.
type Repo struct {
	Dir string // the clone, e.g. <dataDir>/repos/tile-<stack>-<slug>-<hash>
	// Root is the folder every clone sits in (<dataDir>/repos). Checkout
	// refuses a Dir that is not one folder directly inside it: a half-written
	// clone is deleted, and a Dir of Root itself would delete every clone.
	Root   string
	URL    string
	Branch string // "" = the remote's default

	// Auth returns extra environment lines (GIT_CONFIG_COUNT/KEY/VALUE…) that
	// authenticate the fetch, a connection's clone access. Environment, never
	// argv or the URL: argv is world-readable in /proc and the URL is echoed
	// into the job log.
	Auth func(ctx context.Context) []string
}

func (r Repo) env(ctx context.Context) []string {
	env := append(os.Environ(), "GIT_ALLOW_PROTOCOL="+allowProtocol, "GIT_TERMINAL_PROMPT=0")
	if r.Auth != nil {
		env = append(env, r.Auth(ctx)...)
	}
	return env
}

// Checkout makes r.Dir a clone of r.URL checked out at ref, and returns the
// resulting commit SHA. ref is a branch or a commit; "" means r.Branch, and no
// branch means the remote's default. The clone tracks r.Branch and ref is
// fetched on top, so a commit that is not on that branch still resolves. Not
// shallow on purpose: a promote checks out a commit that may not be on the
// branch. w receives git's own output (the job log).
func (r Repo) Checkout(ctx context.Context, ref string, w io.Writer) (string, error) {
	if ref == "" {
		ref = r.Branch
	}
	if ref == "" {
		ref = "HEAD"
	}
	if err := refOK(ref); err != nil {
		return "", err
	}
	if err := r.inRoot(); err != nil {
		return "", err
	}
	env := r.env(ctx)
	run := func(dir string, args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = env
		cmd.Stdout = w
		cmd.Stderr = w
		return cmd.Run()
	}
	_, err := os.Stat(filepath.Join(r.Dir, ".git"))
	if err == nil {
		// A clone of another repo (its owner was bound to a new URL since)
		// would keep fetching the old one: start over.
		if out, gerr := r.git(ctx, "config", "--get", "remote.origin.url"); gerr != nil || strings.TrimSpace(string(out)) != r.URL {
			err = errors.New("another repo")
		}
	}
	if err != nil {
		// Anything there without a .git is a half-written clone from a killed
		// job, and not repairable. Start over.
		_ = os.RemoveAll(r.Dir)
		args := []string{"clone", "--single-branch"}
		if r.Branch != "" {
			args = append(args, "--branch", r.Branch)
		}
		// "--" so a URL beginning with a dash is not read as a flag.
		if err := run("", append(args, "--", r.URL, r.Dir)...); err != nil {
			return "", fmt.Errorf("git clone: %w", err)
		}
	} else if err := dropLocks(filepath.Join(r.Dir, ".git")); err != nil {
		return "", err
	}
	// No auto gc: a detached one outlives the job and holds locks the next
	// checkout's dropLocks would remove.
	if err := run(r.Dir, "-c", "gc.auto=0", "-c", "maintenance.auto=false", "fetch", "origin", ref); err != nil {
		return "", fmt.Errorf("git fetch %s: %w", ref, err)
	}
	// Detached HEAD by design: one path for a branch and a raw SHA; -f
	// throws away whatever the last build left in the tree.
	if err := run(r.Dir, "-c", "advice.detachedHead=false", "checkout", "-f", "FETCH_HEAD"); err != nil {
		return "", fmt.Errorf("git checkout: %w", err)
	}
	// Housekeeping the fetch above skipped: gc --auto only does work past git's
	// thresholds, and runs here in the foreground so it never outlives the job.
	// Best effort: a failed gc never fails a checkout.
	_ = run(r.Dir, "-c", "gc.autoDetach=false", "gc", "--auto", "--quiet")
	out, err := r.git(ctx, "rev-parse", "HEAD")
	return strings.TrimSpace(string(out)), err
}

// inRoot: Dir is exactly one folder inside Root, never Root itself (an
// empty path element) nor anything outside or deeper.
func (r Repo) inRoot() error {
	if r.Root == "" || r.Dir == "" {
		return fmt.Errorf("git: clone %q has no repos root", r.Dir)
	}
	rel, err := filepath.Rel(filepath.Clean(r.Root), filepath.Clean(r.Dir))
	if err != nil || rel == "." || rel == ".." || strings.ContainsRune(rel, filepath.Separator) {
		return fmt.Errorf("git: clone %q is not a folder directly inside %q", r.Dir, r.Root)
	}
	return nil
}

// dropLocks removes the *.lock files a killed git left in a clone (a
// superseded job's checkout is SIGKILLed mid-write; so is one in a crash).
// Left there, every later fetch or checkout of the clone fails for good.
// Safe only because no other git writes the clone at the same time: every
// Checkout caller holds the orchestrator's per-dir repoLock, and one stackr
// process owns a data dir.
func dropLocks(gitDir string) error {
	return filepath.WalkDir(gitDir, func(p string, d os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == "objects":
			return filepath.SkipDir
		case d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".lock"):
			return os.Remove(p)
		}
		return nil
	})
}

// ReadFile returns path as it is at commit, from the local clone.
func (r Repo) ReadFile(ctx context.Context, commit, path string) ([]byte, error) {
	if err := refOK(commit); err != nil {
		return nil, err
	}
	return r.git(ctx, "show", commit+":"+filepath.ToSlash(filepath.Clean(path)))
}

// ListTree lists the files under the folder path at commit, recursively and
// repo-relative; nil when path is not a folder there.
func (r Repo) ListTree(ctx context.Context, commit, path string) ([]string, error) {
	if err := refOK(commit); err != nil {
		return nil, err
	}
	// The trailing slash makes ls-tree match a folder only, never a file.
	out, err := r.git(ctx, "ls-tree", "-r", "-z", "--name-only", commit, "--", filepath.ToSlash(filepath.Clean(path))+"/")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Split(string(out), "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// IsDir reports whether path is a folder at commit. A git failure (no clone,
// unknown commit) is an error, never "not a folder".
func (r Repo) IsDir(ctx context.Context, commit, path string) (bool, error) {
	if err := refOK(commit); err != nil {
		return false, err
	}
	// ls-tree prints nothing, and succeeds, for a path that is not there.
	out, err := r.git(ctx, "ls-tree", "-z", commit, "--", filepath.ToSlash(filepath.Clean(path)))
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(string(out), "040000 tree "), nil
}

// Has reports whether the clone holds commit.
func (r Repo) Has(ctx context.Context, commit string) bool {
	if refOK(commit) != nil {
		return false
	}
	_, err := r.git(ctx, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// ChangedPaths lists the paths that differ between two commits of the local
// clone (for watch_paths).
func (r Repo) ChangedPaths(ctx context.Context, from, to string) ([]string, error) {
	for _, ref := range []string{from, to} {
		if err := refOK(ref); err != nil {
			return nil, err
		}
	}
	out, err := r.git(ctx, "diff", "--name-only", from, to, "--")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// git runs a read-only command in the clone and returns stdout; stderr goes
// into the error.
func (r Repo) git(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.Dir}, args...)...)
	cmd.Env = r.env(ctx)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// refOK refuses a ref git would read as a flag.
func refOK(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return fmt.Errorf("git: bad ref %q", ref)
	}
	return nil
}

// Branches lists the remote's branch names. Best effort: nil on any error
// (bad URL, auth, timeout), because it only fills a picker.
func (r Repo) Branches(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", "--", r.URL)
	cmd.Env = r.env(ctx)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var branches []string
	for _, line := range strings.Split(string(out), "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			branches = append(branches, strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/"))
		}
	}
	return branches
}

// Head asks the remote for the tip of r.Branch, or of its default branch
// when r.Branch is "": the branch's name and its commit. No clone.
func (r Repo) Head(ctx context.Context) (branch, sha string, err error) {
	ref := "HEAD"
	if r.Branch != "" {
		ref = "refs/heads/" + r.Branch
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--symref", "--", r.URL, ref)
	cmd.Env = r.env(ctx)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("git ls-remote: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	branch = r.Branch
	for _, line := range strings.Split(string(out), "\n") {
		left, _, ok := strings.Cut(line, "\t")
		switch {
		case !ok:
		case strings.HasPrefix(left, "ref: "):
			branch = strings.TrimPrefix(strings.TrimPrefix(left, "ref: "), "refs/heads/")
		default:
			sha = left
		}
	}
	if sha == "" || branch == "" {
		return "", "", fmt.Errorf("git: %s has no %s", r.URL, ref)
	}
	return branch, sha, nil
}

// Under resolves a repo-relative path (build context, Dockerfile, a files:
// entry) against the clone and refuses one that lands outside it: the clone
// sits next to every other clone, and "../../keys" with a one-line COPY
// Dockerfile would ship them all in a pullable image. Symlinks are resolved
// first, since a clone keeps them: a committed "ctx -> ../.." is as far out
// as "../..". The answer is the resolved path, so the caller (docker) never
// follows a link again.
func (r Repo) Under(p string) (string, error) {
	full := filepath.Join(r.Dir, filepath.FromSlash(p))
	root, err := resolve(r.Dir)
	if err != nil {
		return "", err
	}
	real, err := resolve(full)
	if err != nil {
		return "", err
	}
	if !inside(r.Dir, full) || !inside(root, real) {
		return "", fmt.Errorf("%q escapes the repo", p)
	}
	return real, nil
}

// resolve is filepath.EvalSymlinks for a path that may not exist yet: the
// deepest existing parent is resolved and the missing rest joined on. A
// dangling link is an error, not a missing path: its target could appear.
func resolve(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	tail := ""
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(real, tail), nil
		}
		if _, lerr := os.Lstat(p); !errors.Is(lerr, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		tail = filepath.Join(filepath.Base(p), tail)
		p = parent
	}
}

func inside(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ImageName is the name of an image built from a commit: repoName (the
// caller's, e.g. stkr/<org>_<stack>_<tile>) tagged with the short SHA, so a
// rollback or promote is "run that commit's image". No SHA: the job id.
// extract: dropped the "tag exists locally, append -jobID[:8]" rebuild rule,
// belongs in leaf/image (a second build of one commit must not overwrite the
// image an older release points at).
func ImageName(repoName, sha, jobID string) string {
	if len(sha) >= 7 {
		return repoName + ":" + sha[:7]
	}
	return repoName + ":" + jobID[:min(8, len(jobID))]
}
