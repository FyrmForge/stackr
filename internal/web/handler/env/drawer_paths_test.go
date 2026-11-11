package env_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The drawer's actions answer on the renamed paths, and the old ones are gone.
func TestRenamedActionPaths(t *testing.T) {
	s := webtest.New(t)
	operated(t, s)
	for _, p := range []string{"accept-restore", "failover", "rollback-upgrade"} {
		if rec := s.Do(t, "POST", "/acme/shop/dev/-/instances/pg/"+p, url.Values{}); rec.Code == 404 || rec.Code == 405 {
			t.Errorf("%s = %d, want a handler", p, rec.Code)
		}
	}
	for _, p := range []string{"promote-restore", "promote-standby", "major-rollback"} {
		if rec := s.Do(t, "POST", "/acme/shop/dev/-/instances/pg/"+p, url.Values{}); rec.Code != 404 && rec.Code != 405 {
			t.Errorf("old path %s = %d, want gone", p, rec.Code)
		}
	}
}

// An update says whether a job runs or the change waits for the next deploy.
func TestPGUpdateNote(t *testing.T) {
	s := webtest.New(t)
	operated(t, s)
	rec := s.Do(t, "POST", "/acme/shop/dev/-/instances/pg/version", url.Values{})
	body := rec.Body.String()
	if rec.Code == 200 && !strings.Contains(body, "Saved. Applies on the next deploy.") && !strings.Contains(body, "Progress shows under Last job.") {
		t.Errorf("update note missing:\n%s", body)
	}
}

// The live stream carries the last job as its own region.
func TestInstanceLiveStreamHasJobRegion(t *testing.T) {
	s := webtest.New(t)
	pg := operated(t, s)
	page := s.Do(t, "GET", "/acme/shop/dev/-/instances/"+pg.Slug, nil).Body.String()
	if !strings.Contains(page, `sse-swap="job"`) {
		t.Errorf("no job region:\n%s", page)
	}
}
