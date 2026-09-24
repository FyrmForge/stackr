package tile_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

const drawer = "/acme/shop/dev/-/tiles/api"

// Every tab of an image tile answers a drawer fragment, never a page.
func TestEveryTabServes(t *testing.T) {
	s := webtest.New(t)
	for _, tab := range []string{
		"status",
		"logs",
		"domains",
		"env",
		"settings",
		"jobs",
		"image",
		"backups",
		"nonsense",
	} {
		rec := s.Do(t, "GET", drawer+"?tab="+tab, nil)
		body := rec.Body.String()
		if rec.Code != 200 || !strings.HasPrefix(body, `<div id="drawer-view">`) || strings.Contains(body, "<html") {
			t.Errorf("tab %s = %d\n%s", tab, rec.Code, body)
		}
	}
}

// An action runs its verb and answers its tab; a refusal shows over the
// tab with 422 so htmx still swaps it.
func TestActions(t *testing.T) {
	s := webtest.New(t)
	rec := s.Do(t, "POST", drawer+"/deploy", url.Values{})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "deploy queued") ||
		!strings.Contains(rec.Body.String(), "/-/jobs/") {
		t.Errorf("deploy = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/pause", url.Values{"paused": {"true"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "only cron tiles") {
		t.Errorf("pause an image tile = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/env", url.Values{"env_json": {`{"GREETING":"hi"}`}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "GREETING") ||
		!strings.Contains(rec.Body.String(), "saved") {
		t.Errorf("env = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/settings", url.Values{"cpu_limit": {"lots"}, "mem_limit_mb": {"256"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "must be a number") ||
		!strings.Contains(rec.Body.String(), `value="256"`) {
		t.Errorf("bad settings = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/settings", url.Values{"cpu_limit": {"0.5"}, "mem_limit_mb": {""}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `value="0.5"`) {
		t.Errorf("settings = %d\n%s", rec.Code, rec.Body)
	}
}

// Writes need the CSRF token like every form on the site.
func TestActionNeedsCSRF(t *testing.T) {
	s := webtest.New(t)
	if rec := s.DoNoCSRF(t, "POST", drawer+"/deploy"); rec.Code != 403 && rec.Code != 400 {
		t.Errorf("deploy without CSRF = %d", rec.Code)
	}
}
