// Package gitlog is a list of recent commits on one branch, from GitHub or a
// local clone, in one shape.
package gitlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Commit is one commit as the log shows it.
type Commit struct {
	SHA       string
	Message   string // first line
	Author    string
	Login     string // GitHub login, "" locally
	AvatarURL string // "" locally: the row shows initials
	URL       string // the commit on GitHub, "" locally
	When      time.Time
}

// Short is the 7-char hash.
func (c Commit) Short() string {
	if len(c.SHA) > 7 {
		return c.SHA[:7]
	}
	return c.SHA
}

// Local reads the last n commits of ref from a clone at dir.
func Local(ctx context.Context, dir, ref string, n int) ([]Commit, error) {
	args := []string{"log", "-n", fmt.Sprint(n), "--format=%H%x1f%an%x1f%aI%x1f%s"}
	if ref != "" {
		args = append(args, ref)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("git log: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var commits []Commit
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		f := strings.SplitN(string(line), "\x1f", 4)
		if len(f) < 4 {
			continue
		}
		when, _ := time.Parse(time.RFC3339, f[2])
		commits = append(commits, Commit{SHA: f[0], Author: f[1], When: when, Message: f[3]})
	}
	return commits, nil
}

// One reads a single commit from a clone at dir.
func One(ctx context.Context, dir, sha string) (Commit, error) {
	cs, err := Local(ctx, dir, sha, 1)
	if err != nil {
		return Commit{}, err
	}
	if len(cs) == 0 {
		return Commit{}, fmt.Errorf("git log: %s not found", sha)
	}
	return cs[0], nil
}

// Behind counts the commits on ref that sha does not have. onBranch is false
// when sha is not an ancestor of ref (force push, rebase, hotfix branch), and
// behind is then meaningless.
func Behind(ctx context.Context, dir, sha, ref string) (behind int, onBranch bool, err error) {
	anc := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", sha, ref)
	anc.Dir = dir
	if err := anc.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return 0, false, nil
		}
		return 0, false, err
	}
	cmd := exec.CommandContext(ctx, "git", "rev-list", "--count", sha+".."+ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return 0, true, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return n, true, err
}
