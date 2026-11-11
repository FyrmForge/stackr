package web_test

import (
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
