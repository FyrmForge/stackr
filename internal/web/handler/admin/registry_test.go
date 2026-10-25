package admin_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The Registry tab is an admin's: it opens with the cleanup section and the
// "Clean up now" button, a registry that does not answer is the tab's text,
// and an org owner is refused both the tab and the cleanup.
func TestAdminRegistryTab(t *testing.T) {
	s := webtest.New(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/-/admin/registry"},
		{"POST", "/-/admin/registry/clean"},
	} {
		if rec := s.Do(t, c.method, c.path, nil); rec.Code != http.StatusForbidden {
			t.Errorf("owner %s %s = %d, want 403", c.method, c.path, rec.Code)
		}
	}
	root := s.Session(t, s.User(t, "root@x.test", true))
	rec := s.As(t, root, "GET", "/-/admin/registry", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `id="tab-registry"`) || !strings.Contains(body, "Last cleanup: none yet.") ||
		!strings.Contains(body, "Clean up now") {
		t.Fatalf("registry tab = %d\n%s", rec.Code, body)
	}
	if !strings.Contains(body, "the registry did not answer") && !strings.Contains(body, " in 0 versions") {
		t.Errorf("neither the totals nor the registry's failure:\n%s", body)
	}
}
