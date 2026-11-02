package api_test

import (
	"encoding/json"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// Another org's job is a 404 on every org job route, whatever the caller's
// role in its own org: read, log, events and cancel.
func TestJobsStayInTheirOrg(t *testing.T) {
	w := newWorld(t)
	_, body := w.do(t, w.owner, "POST", "/orgs/acme/stacks/shop/envs/dev/tiles/api/deploy", "")
	var j service.Job
	if err := json.Unmarshal([]byte(body), &j); err != nil || j.ID == "" {
		t.Fatalf("deploy = %s", body)
	}
	for _, r := range [][2]string{
		{"GET", ""}, {"GET", "/log?offset=0"}, {"GET", "/events"}, {"POST", "/cancel"},
	} {
		if code, out := w.do(t, w.stranger, r[0], "/orgs/other/jobs/"+j.ID+r[1], ""); code != 404 {
			t.Errorf("%s job%s through another org = %d %s, want 404", r[0], r[1], code, out)
		}
		if code, _ := w.do(t, w.stranger, r[0], "/orgs/acme/jobs/"+j.ID+r[1], ""); code != 404 {
			t.Errorf("%s job%s as a stranger to its org = %d, want 404", r[0], r[1], code)
		}
	}
	if code, _ := w.do(t, w.owner, "GET", "/orgs/acme/jobs/"+j.ID, ""); code != 200 {
		t.Errorf("own job = %d, want 200", code)
	}
}
