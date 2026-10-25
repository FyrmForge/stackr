package admin_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The Security log tab: admins only, newest first, one row per act with its
// detail folded in, and the action filter narrows it.
func TestAdminSecurityTab(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	if rec := s.Do(t, "GET", "/-/admin?tab=security", nil); rec.Code != http.StatusForbidden {
		t.Errorf("owner opens the tab = %d, want 403", rec.Code)
	}
	if err := s.Orch.LoginRefused(ctx, "dev@smoke.dev", service.RequestMeta{IP: "203.0.113.9", UserAgent: "curl"}); err != nil {
		t.Fatal(err)
	}
	root := s.Session(t, s.User(t, "root@x.test", true))
	rec := s.As(t, root, "GET", "/-/admin?tab=security", nil)
	body := rec.Body.String()
	for _, want := range []string{`id="tab-security"`, "sign-in rate limited", "dev@smoke.dev", "203.0.113.9", "refused"} {
		if rec.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("tab = %d, no %q in\n%s", rec.Code, want, body)
		}
	}
	body = s.As(t, root, "GET", "/-/admin?tab=security&action=key.mint", nil).Body.String()
	if strings.Contains(body, "203.0.113.9") || !strings.Contains(body, "Nothing matches") {
		t.Errorf("action filter kept the other rows:\n%s", body)
	}
}

// The Security tab shows the Master key section; a wrong password is refused
// and nothing is written.
func TestAdminMasterKeyRotateWrongPassword(t *testing.T) {
	s := webtest.New(t)
	uid := s.User(t, "root@x.test", true)
	if err := s.Orch.SetPassword(context.Background(), uid, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	root := s.Session(t, uid)
	if body := s.As(t, root, "GET", "/-/admin?tab=security", nil).Body.String(); !strings.Contains(body, `id="master-key"`) || !strings.Contains(body, "Master key") {
		t.Fatalf("no Master key section in\n%s", body)
	}
	rec := s.As(t, root, "POST", "/-/admin/security/master-key/rotate", url.Values{"password": {"wrong"}})
	if rec.Code != http.StatusUnprocessableEntity || strings.Contains(rec.Body.String(), "restarting") {
		t.Errorf("wrong password = %d\n%s", rec.Code, rec.Body.String())
	}
}
