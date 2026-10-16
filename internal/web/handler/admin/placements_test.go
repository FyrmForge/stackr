package admin_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// Every placement route is node.manage: an org owner reaches none, and the
// gap's secret field is the server scope's, so it is admin-gated too.
func TestAdminPlacementsOwnerRefused(t *testing.T) {
	s := webtest.New(t)
	s.User(t, "root@x.test", true)
	for _, c := range []struct{ method, path string }{
		{"GET", "/-/admin/placements"}, {"POST", "/-/admin/placements"},
		{"POST", "/-/admin/placements/eu/open"}, {"POST", "/-/admin/placements/eu/delete"},
		{"GET", "/-/admin/placements/eu/move"}, {"POST", "/-/admin/placements/eu/move"},
		{"GET", "/-/admin/placements/eu/progress"}, {"POST", "/-/admin/placements/eu/retry"},
		{"POST", "/-/admin/config/gaps/secret"},
	} {
		if rec := s.Do(t, c.method, c.path, url.Values{"name": {"eu"}}); rec.Code != http.StatusForbidden {
			t.Errorf("owner %s %s = %d, want 403", c.method, c.path, rec.Code)
		}
	}
}

// A gap's secret field with no value is a refusal on the Config tab and
// writes nothing.
func TestAdminGapSecretNeedsValue(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	rec := s.As(t, root, "POST", "/-/admin/config/gaps/secret", url.Values{"secret.db.password": {""}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "enter the value") {
		t.Errorf("empty gap value = %d\n%s", rec.Code, rec.Body)
	}
}
