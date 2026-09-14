package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// CIVerdict is the merged state of everything CI reported on a commit.
type CIVerdict string

const (
	CIPassing CIVerdict = "success"
	CIFailing CIVerdict = "failure"
	CIPending CIVerdict = "pending"
	// CINone: no check runs and no commit statuses at all, the repo may
	// simply have no CI. Callers decide how long to wait before shrugging.
	CINone CIVerdict = "none"
)

// ErrNoChecksPerm: the installation lacks checks:read. New connectors get it
// from the manifest; existing ones need the owner to approve the updated
// permissions on GitHub once.
var ErrNoChecksPerm = errors.New("connector lacks checks:read; approve the updated app permissions on GitHub")

// RepoFull extracts "owner/repo" from an https or ssh GitHub URL
// ("" if it isn't one).
func RepoFull(gitURL string) string {
	s := strings.TrimSpace(gitURL)
	switch {
	case strings.HasPrefix(s, "https://github.com/"):
		s = strings.TrimPrefix(s, "https://github.com/")
	case strings.HasPrefix(s, "git@github.com:"):
		s = strings.TrimPrefix(s, "git@github.com:")
	default:
		return ""
	}
	return strings.Trim(strings.TrimSuffix(s, ".git"), "/")
}

type checkRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued | in_progress | completed
	Conclusion string `json:"conclusion"` // success | failure | neutral | cancelled | skipped | timed_out | action_required
}

type commitStatus struct {
	Context string `json:"context"`
	State   string `json:"state"` // success | failure | error | pending
}

// CIState merges GitHub's two reporting channels for one commit: check runs
// (Actions and other check-suite apps) and legacy commit statuses. Passing
// means every check run completed success/neutral/skipped AND the combined
// status is green.
func (c *Client) CIState(ctx context.Context, cn *repo.Connector, repoFull, sha string) (CIVerdict, string, error) {
	token, err := c.Token(ctx, cn)
	if err != nil {
		return CIPending, "", err
	}

	var runs struct {
		CheckRuns []checkRun `json:"check_runs"`
	}
	// 403 sniffed from tokAPI's error string, it folds the HTTP
	// status into one error and unwrapping it isn't worth a second API helper.
	if err := c.tokAPI(ctx, token, "GET",
		"/repos/"+repoFull+"/commits/"+url.PathEscape(sha)+"/check-runs?filter=latest&per_page=100",
		nil, &runs); err != nil {
		if strings.Contains(err.Error(), "403") {
			return CIPending, "", ErrNoChecksPerm
		}
		return CIPending, "", err
	}

	var combined struct {
		State    string         `json:"state"`
		Statuses []commitStatus `json:"statuses"`
	}
	if err := c.tokAPI(ctx, token, "GET",
		"/repos/"+repoFull+"/commits/"+url.PathEscape(sha)+"/status",
		nil, &combined); err != nil {
		return CIPending, "", err
	}
	v, detail := mergeVerdict(runs.CheckRuns, combined.Statuses)
	return v, detail, nil
}

// mergeVerdict folds both channels into one answer. Failure anywhere wins,
// then pending, then "nothing reported at all".
func mergeVerdict(runs []checkRun, statuses []commitStatus) (CIVerdict, string) {
	pending := false
	for _, r := range runs {
		if r.Status != "completed" {
			pending = true
			continue
		}
		switch r.Conclusion {
		case "success", "neutral", "skipped":
		default:
			return CIFailing, fmt.Sprintf("check %q: %s", r.Name, r.Conclusion)
		}
	}
	for _, s := range statuses {
		switch s.State {
		case "failure", "error":
			return CIFailing, fmt.Sprintf("status %q: %s", s.Context, s.State)
		case "pending":
			pending = true
		}
	}
	if pending {
		return CIPending, ""
	}
	if len(runs) == 0 && len(statuses) == 0 {
		return CINone, ""
	}
	return CIPassing, ""
}
