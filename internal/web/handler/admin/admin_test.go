package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The admin drawer: admin-only, every tab renders, the settings form
// writes what changed, users flip, a panel backup streams its job.
func TestAdminDrawer(t *testing.T) {
	stream.PollEvery = 10 * time.Millisecond
	s := webtest.New(t)
	ctx := context.Background()
	if rec := s.Do(t, "GET", "/-/admin", nil); rec.Code != http.StatusForbidden {
		t.Errorf("owner opens admin = %d, want 403", rec.Code)
	}
	root := s.Session(t, s.User(t, "root@x.test", true))
	for tab, want := range map[string]string{
		"settings": `id="admin-settings"`, "users": "owner@acme.test", "update": "/-/admin/update/check",
		"caddy": `name="proxy_custom"`, "backups": "No panel backups yet",
	} {
		rec := s.As(t, root, "GET", "/-/admin?tab="+tab, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("tab %s = %d, no %s in\n%s", tab, rec.Code, want, rec.Body)
		}
	}
	if body := s.As(t, root, "GET", "/acme", nil).Body.String(); !strings.Contains(body, `hx-get="/-/admin?tab=settings"`) {
		t.Error("no admin button in the nav")
	}

	rec := s.As(t, root, "POST", "/-/admin/settings", url.Values{"workers": {"3"}, "cpu_limit": {""}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Saved.") {
		t.Fatalf("save = %d %s", rec.Code, rec.Body)
	}
	if v, _ := s.Orch.Setting(ctx, "workers"); v != "3" {
		t.Errorf("workers = %q", v)
	}
	if rec := s.As(t, root, "POST", "/-/admin/settings", url.Values{"workers": {"lots"}}); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not a whole number") {
		t.Errorf("bad value = %d %s", rec.Code, rec.Body)
	}

	var owner string
	us, _ := s.Orch.Users(ctx)
	for _, u := range us {
		if u.Email == "owner@acme.test" {
			owner = u.ID
		}
	}
	if rec := s.As(t, root, "POST", "/-/admin/users/"+owner+"/admin?admin=true", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Remove admin") {
		t.Errorf("make admin = %d", rec.Code)
	}

	rec = s.As(t, root, "POST", "/-/admin/backups", nil)
	m := regexp.MustCompile(`sse-connect="(/-/jobs/[^"]+/events)"`).FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		t.Fatalf("backup now = %d %s", rec.Code, rec.Body)
	}
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("GET", m[1], nil).WithContext(cctx)
	req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: root})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "event: update") {
		t.Errorf("job stream:\n%s", w.Body)
	}

	// A fresh load of ?drawer=admin opens it on any page.
	req = httptest.NewRequest("GET", "/acme?drawer=admin&tab=users", nil)
	req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: root})
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `hx-get="/-/admin?tab=users"`) {
		t.Errorf("fresh load has no admin drawer:\n%s", w.Body)
	}
}
