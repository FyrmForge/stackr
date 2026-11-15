package admin_test

import (
	"net/http"
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
	if rec := s.As(t, root, "GET", "/-/admin/find?q=", nil); strings.TrimSpace(rec.Body.String()) != "" {
		t.Errorf("empty query = %q", rec.Body)
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
	for _, w := range []string{`type="search"`, `aria-label="Find a setting"`, `hx-trigger="input changed delay:250ms, search"`, `id="admin-find-results"`, `aria-live="polite"`} {
		if !strings.Contains(page, w) {
			t.Errorf("page: no %s", w)
		}
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
}
