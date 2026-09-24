// Package git clones and checks out source repositories. It shells out to the
// git binary. No store handle, no Docker handle, no decisions about a deploy.
package git

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	Dir    string // the clone, e.g. <dataDir>/repos/<tile-id>
	URL    string
	Branch string // "" = the remote's default

	// Auth returns extra environment lines (GIT_CONFIG_COUNT/KEY/VALUE…) that
	// authenticate the fetch, e.g. githubapp.CloneAuth. Environment, never
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
	env := r.env(ctx)
	run := func(dir string, args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, w, w
		return cmd.Run()
	}
	if _, err := os.Stat(filepath.Join(r.Dir, ".git")); err != nil {
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
	}
	if err := run(r.Dir, "fetch", "origin", ref); err != nil {
		return "", fmt.Errorf("git fetch %s: %w", ref, err)
	}
	// Detached HEAD by design: one path for a branch and a raw SHA; -f
	// throws away whatever the last build left in the tree.
	if err := run(r.Dir, "checkout", "-f", "FETCH_HEAD"); err != nil {
		return "", fmt.Errorf("git checkout: %w", err)
	}
	out, err := r.git(ctx, "rev-parse", "HEAD")
	return strings.TrimSpace(string(out)), err
}

// ReadFile returns path as it is at commit, from the local clone.
func (r Repo) ReadFile(ctx context.Context, commit, path string) ([]byte, error) {
	if err := refOK(commit); err != nil {
		return nil, err
	}
	return r.git(ctx, "show", commit+":"+filepath.ToSlash(filepath.Clean(path)))
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

// Under resolves a repo-relative path (build context, Dockerfile, a files:
// entry) against the clone and refuses one that lands outside it: the clone
// sits next to every other clone, and "../../keys" with a one-line COPY
// Dockerfile would ship them all in a pullable image.
func (r Repo) Under(p string) (string, error) {
	full := filepath.Join(r.Dir, filepath.FromSlash(p))
	rel, err := filepath.Rel(r.Dir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q escapes the repo", p)
	}
	return full, nil
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
