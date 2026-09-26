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
		"runs",
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
	rec = s.Do(t, "POST", drawer+"/env", url.Values{"env": {"GREETING=hi\nNAME=bob"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `aria-label="Delete GREETING"`) ||
		!strings.Contains(rec.Body.String(), "saved") {
		t.Errorf("env = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/env", url.Values{"drop": {"GREETING"}})
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "Delete GREETING") ||
		!strings.Contains(rec.Body.String(), "Delete NAME") {
		t.Errorf("drop = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/env", url.Values{"env": {"NAME=bob\noops"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "line 2: want KEY=VALUE") {
		t.Errorf("bad env = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/settings", url.Values{"cpu_limit": {"lots"}, "mem_limit_mb": {"256"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "must be a number") ||
		!strings.Contains(rec.Body.String(), `value="256"`) || !strings.Contains(rec.Body.String(), `value="lots"`) {
		t.Errorf("bad settings = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/settings", url.Values{"cpu_limit": {"0.5"}, "mem_limit_mb": {""}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `value="0.5"`) {
		t.Errorf("settings = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/settings", url.Values{"restart_policy": {"sometimes"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `id="error-restart_policy" data-has-error="true"`) {
		t.Errorf("refused restart policy = %d\n%s", rec.Code, rec.Body)
	}
}

// Writes need the CSRF token like every form on the site.
func TestActionNeedsCSRF(t *testing.T) {
	s := webtest.New(t)
	if rec := s.DoNoCSRF(t, "POST", drawer+"/deploy"); rec.Code != 403 && rec.Code != 400 {
		t.Errorf("deploy without CSRF = %d", rec.Code)
	}
}
