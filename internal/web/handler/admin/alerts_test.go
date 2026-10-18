package admin_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The Alerts tab saves the webhook, sends a test, shows the last delivery
// and turns alerts off; an org owner is refused.
func TestAdminAlertsTab(t *testing.T) {
	s := webtest.New(t)
	if rec := s.Do(t, "POST", "/-/admin/alerts/test", nil); rec.Code != http.StatusForbidden {
		t.Errorf("owner test = %d, want 403", rec.Code)
	}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(hook.Close)
	root := s.Session(t, s.User(t, "root@x.test", true))
	if rec := s.As(t, root, "POST", "/-/admin/alerts", url.Values{"url": {"nope"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad url = %d %s", rec.Code, rec.Body)
	}
	rec := s.As(t, root, "POST", "/-/admin/alerts", url.Values{"url": {hook.URL + "/x"}, "secret": {"s"}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Saved.") || !strings.Contains(body, "Send test") {
		t.Fatalf("save = %d %s", rec.Code, body)
	}
	rec = s.As(t, root, "POST", "/-/admin/alerts/test", nil)
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Test alert delivered.") || !strings.Contains(body, "Last delivery: ok") {
		t.Fatalf("test = %d %s", rec.Code, body)
	}
	rec = s.As(t, root, "POST", "/-/admin/alerts/clear", nil)
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Alerts are off.") || strings.Contains(body, "Send test") {
		t.Fatalf("clear = %d %s", rec.Code, body)
	}
}
