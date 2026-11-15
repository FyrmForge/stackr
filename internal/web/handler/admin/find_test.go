package admin_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The Find a setting box: admin-only hits, links that open a section with
// the find key, which marks that row and nothing else.
func TestAdminFind(t *testing.T) {
	s := webtest.New(t)
	if rec := s.Do(t, "GET", "/-/admin/find?q=update", nil); rec.Code != http.StatusForbidden {
		t.Errorf("owner finds = %d, want 403", rec.Code)
	}
	root := s.Session(t, s.User(t, "root@x.test", true))

	body := s.As(t, root, "GET", "/-/admin/find?q=update_check", nil).Body.String()
	for _, w := range []string{`href="/-/admin/update?find=update_check"`, `hx-target="#main"`, `hx-push-url="true"`} {
		if !strings.Contains(body, w) {
			t.Errorf("no %s in\n%s", w, body)
		}
	}
	if body := s.As(t, root, "GET", "/-/admin/find?q=stackr+updates", nil).Body.String(); !strings.Contains(body, "<mark>") {
		t.Errorf("no mark in\n%s", body)
	}
	if rec := s.As(t, root, "GET", "/-/admin/find?q=", nil); strings.Contains(rec.Body.String(), "<li>") || !strings.Contains(rec.Body.String(), `hx-swap-oob="true"`) {
		t.Errorf("empty query = %q", rec.Body)
	}
	// an over-long query is cut, not refused
	if rec := s.As(t, root, "GET", "/-/admin/find?q="+strings.Repeat("e", 5000), nil); rec.Code != http.StatusOK {
		t.Errorf("long query = %d", rec.Code)
	}
	// hand-written editors are found by name, linking to the section alone
	if b := s.As(t, root, "GET", "/-/admin/find?q=panel_backup_keep", nil).Body.String(); !strings.Contains(b, `href="/-/admin/backups"`) || strings.Contains(b, "find=panel_backup_keep") {
		t.Errorf("own-editor setting not found in %s", b)
	}
	if b := s.As(t, root, "GET", "/-/admin/find?q=zzzqq", nil).Body.String(); !strings.Contains(b, "No setting matches.") {
		t.Errorf("no hits = %s", b)
	}
	if b := s.As(t, root, "GET", "/-/admin/find?q=e", nil).Body.String(); !strings.Contains(b, "more, keep typing.") {
		t.Errorf("no cap line in %s", b)
	}
	if b := s.As(t, root, "GET", "/-/admin/find?q=%3Cscript%3E", nil).Body.String(); strings.Contains(b, "<script>") {
		t.Errorf("unescaped query in %s", b)
	}
	// a hand-written section is found by name
	if b := s.As(t, root, "GET", "/-/admin/find?q=alerts", nil).Body.String(); !strings.Contains(b, `href="/-/admin/alerts"`) {
		t.Errorf("section hit missing in %s", b)
	}

	// the box sits on the page
	page := s.As(t, root, "GET", "/-/admin/update", nil).Body.String()
	for _, w := range []string{`type="search"`, `aria-label="Find a setting"`, `hx-trigger="input changed delay:250ms, search"`, `id="admin-find-results"`, `maxlength="100"`, `role="status"`} {
		if !strings.Contains(page, w) {
			t.Errorf("page: no %s", w)
		}
	}
	if strings.Contains(page, `id="admin-find-results" aria-live`) {
		t.Error("the results list is a live region")
	}
	if strings.Contains(page, "is-found") || strings.Contains(page, "autofocus") {
		t.Error("a plain section marks a row")
	}
	hit := s.As(t, root, "GET", "/-/admin/update?find=update_check", nil).Body.String()
	if !strings.Contains(hit, `class="is-found"`) || !strings.Contains(hit, `name="update_check"`) || strings.Count(hit, "autofocus") != 1 {
		t.Errorf("hit row not marked in\n%s", hit)
	}
	for _, bogus := range []string{"nope", "workers"} { // unknown, and a key of another section
		if b := s.As(t, root, "GET", "/-/admin/update?find="+bogus, nil).Body.String(); strings.Contains(b, "is-found") || strings.Contains(b, "autofocus") {
			t.Errorf("find=%s marked a row", bogus)
		}
	}
	// a secret's row is marked but never takes focus
	if b := s.As(t, root, "GET", "/-/admin/org-defaults?find=protect_password", nil).Body.String(); !strings.Contains(b, `class="is-found"`) || strings.Contains(b, "autofocus") {
		t.Errorf("secret row: marked and unfocused wanted in\n%s", b)
	}
}

// A save from a ?find= page replaces the URL with the clean section's.
func TestAdminFindSaveCleansURL(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	post := func(current string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/-/admin/settings/jobs", strings.NewReader(url.Values{"workers": {"3"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.Header.Set("X-CSRF-Token", "tok")
		req.AddCookie(&http.Cookie{Name: "__Host-csrf", Value: "tok"})
		req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: root})
		if current != "" {
			req.Header.Set("HX-Current-URL", current)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := post("http://x.test/-/admin/jobs?find=workers"); rec.Code != http.StatusOK || rec.Header().Get("HX-Replace-Url") != "/-/admin/jobs" {
		t.Errorf("from a find URL: %d, %q", rec.Code, rec.Header().Get("HX-Replace-Url"))
	}
	for _, cur := range []string{"", "http://x.test/-/admin/jobs"} {
		if h := post(cur).Header().Get("HX-Replace-Url"); h != "" {
			t.Errorf("current %q sent HX-Replace-Url %q", cur, h)
		}
	}
}
