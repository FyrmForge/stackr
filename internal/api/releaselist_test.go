package api_test

import (
	"encoding/json"
	"testing"
)

// The release list carries each release's source, state and where it runs,
// and an env that is not in the stack is refused.
func TestReleaseListStateAndSource(t *testing.T) {
	w := newWorld(t)
	w.env.Release(t, w.tile.Stack, 1)
	code, body := w.do(t, w.owner, "GET", "/orgs/acme/stacks/shop/releases", "")
	var rows []struct {
		Number int
		Source string
		State  string
		RunsIn string `json:"runs_in"`
	}
	if err := json.Unmarshal([]byte(body), &rows); err != nil || code != 200 || len(rows) != 1 || rows[0].Number != 1 || rows[0].Source == "" || rows[0].State == "" {
		t.Fatalf("list = %d %s (%v)", code, body, err)
	}
	if code, body := w.do(t, w.owner, "GET", "/orgs/acme/stacks/shop/releases?env=nope", ""); code != 400 {
		t.Errorf("unknown env = %d %s, want 400", code, body)
	}
}
