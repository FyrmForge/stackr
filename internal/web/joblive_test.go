package web_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The job-live poll answers 204 with no matching active job, and the slot
// markup with one; ?kind, ?tile and ?node narrow it, at env and stack scope
// and in the panel.
func TestJobLive(t *testing.T) {
	s := webtest.New(t)
	env, stack := "/acme/shop/dev/-/job-live", "/acme/shop/-/job-live"
	get := func(path string) (int, string) {
		r := s.Do(t, "GET", path, nil)
		return r.Code, r.Body.String()
	}
	if code, _ := get(env + "?kind=deploy"); code != http.StatusNoContent {
		t.Fatalf("no job: %d, want 204", code)
	}
	payload := `{"org_id":"` + s.Org + `","stack_id":"` + s.Tile.Stack + `","tile_id":"` + s.Tile.ID + `"}`
	s.JobWith(t, "deploy", "running", payload)
	s.JobWith(t, "backup-node", "running", `{"node_id":"n1"}`)
	for path, want := range map[string]int{
		env + "?kind=deploy":              200,
		env:                               200,
		env + "?kind=env-sync":            204,
		env + "?tile=" + s.Tile.ID:        200,
		env + "?tile=other":               204,
		stack + "?kind=deploy":            200,
		stack + "?kind=deploy&tile=other": 204,
	} {
		if code, _ := get(path); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
	admin := s.Session(t, s.User(t, "root@acme.test", true))
	for path, want := range map[string]int{
		"/-/admin/job-live?kind=backup-node&node=n1": 200,
		"/-/admin/job-live?kind=backup-node&node=n2": 204,
	} {
		if code := s.As(t, admin, "GET", path, nil).Code; code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
	if _, body := get(env + "?kind=deploy"); !strings.Contains(body, `id="job-slot"`) || !strings.Contains(body, "/-/jobs/") {
		t.Errorf("no slot with the job stream:\n%s", body)
	}
}

// A stack-scope slot links the org's job routes (the stack has none). A live
// answer refreshes its tab when the job ends, else asks for the slot again
// (?end=1), which comes back idle with a poller so the next job is picked up.
func TestJobLiveEndsIdleAndRefreshes(t *testing.T) {
	s := webtest.New(t)
	payload := `{"org_id":"` + s.Org + `","stack_id":"` + s.Tile.Stack + `","tile_id":"` + s.Tile.ID + `"}`
	job := s.JobWith(t, "deploy", "queued", payload)
	get := func(path string) string { return s.Do(t, "GET", path, nil).Body.String() }
	stack := "/acme/shop/-/job-live?kind=deploy"
	if b := get(stack); !strings.Contains(b, `href="/acme/-/jobs/`) || strings.Contains(b, "/acme/shop/-/jobs/") {
		t.Errorf("stack slot links a route that does not exist:\n%s", b)
	}
	if b := get(stack + "&refresh=%2Facme%2Fshop%2F-%2Fdrawer%3Ftab%3Dreleases"); !strings.Contains(b, `hx-get="/acme/shop/-/drawer?tab=releases"`) {
		t.Errorf("live slot does not refresh its tab on end:\n%s", b)
	}
	if b := get(stack + "&refresh=%2F%2Fevil.test"); strings.Contains(b, "evil.test") {
		t.Errorf("refresh took an off-site url:\n%s", b)
	}
	b := get(stack)
	if !strings.Contains(b, `hx-trigger="sse:end"`) || !strings.Contains(b, "end=1") {
		t.Errorf("live slot without a tab does not ask for itself on end:\n%s", b)
	}
	// the job ends: the end answer is an idle slot with the poller back
	if err := s.Orch.CancelJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	b = get(stack + "&end=1")
	if !strings.Contains(b, `id="job-slot"`) || !strings.Contains(b, `hx-trigger="every 5s"`) {
		t.Errorf("slot not idle and polling after its job ended:\n%s", b)
	}
}
