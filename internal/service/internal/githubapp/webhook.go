package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// Repository is the part of a delivery's repository object a tile is matched on.
type Repository struct {
	CloneURL      string `json:"clone_url"`
	SSHURL        string `json:"ssh_url"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

// PullRequest is a pull_request delivery. Actions: opened/reopened create
// the pr-<number> env, synchronize redeploys it, closed tears it down.
type PullRequest struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"` // the commit GitHub announced; pin to it, not the branch tip
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository Repository `json:"repository"`
}

// Push is a push delivery.
type Push struct {
	Ref     string `json:"ref"`     // refs/heads/<branch>
	After   string `json:"after"`   // head commit SHA
	Deleted bool   `json:"deleted"` // branch/tag deletion push
	Commits []struct {
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"commits"`
	Repository Repository `json:"repository"`
}

// ChangedFiles flattens the push's touched paths. Empty when the payload
// carries no commit list (force pushes, very large pushes): callers must
// treat that as "anything could have changed", never as "nothing changed".
func (p *Push) ChangedFiles() []string {
	var out []string
	for _, c := range p.Commits {
		out = append(out, c.Added...)
		out = append(out, c.Removed...)
		out = append(out, c.Modified...)
	}
	return out
}

// Event is what a verified delivery amounts to. At most one of PR or Push is
// set; both nil is an event nobody handles, which is not an error.
type Event struct {
	PR   *PullRequest
	Push *Push
}

// ErrBadSignature → 401, ErrBadPayload → 400.
var (
	ErrBadSignature = errors.New("bad signature")
	ErrBadPayload   = errors.New("bad payload")
)

// Receive verifies a delivery against the connector's webhook secret, then
// decodes it. event is X-GitHub-Event, signature is X-Hub-Signature-256, body
// the raw bytes read once by the caller under a 1 MiB limit: the same bytes
// feed the MAC and the decode. Nothing is decoded before the MAC passes.
// extract: the connector lookup (no secret → 404 before the body is read) is
// the route's; what a delivery triggers is flow/promote and leaf/environment.
func Receive(event, signature string, body []byte, secret string) (Event, error) {
	if !validSignature(secret, signature, body) {
		return Event{}, ErrBadSignature
	}
	switch event {
	case "pull_request":
		var p PullRequest
		if err := json.Unmarshal(body, &p); err != nil || p.Number == 0 {
			return Event{}, ErrBadPayload
		}
		return Event{PR: &p}, nil
	case "push":
		var p Push
		if err := json.Unmarshal(body, &p); err != nil {
			return Event{}, ErrBadPayload
		}
		return Event{Push: &p}, nil
	}
	return Event{}, nil // installation, check_suite, …: ignored
}

// validSignature checks GitHub's X-Hub-Signature-256 over the raw body. An
// empty secret never validates: a half-configured connector must be inert,
// not an open deploy trigger. Keep the guard here, not in a caller.
func validSignature(secret, header string, body []byte) bool {
	if secret == "" || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(strings.TrimPrefix(header, "sha256=")))
}
