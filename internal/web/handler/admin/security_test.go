package admin_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/handler/admin"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The Security log tab: admins only, newest first, one row per act with its
// detail folded in, and the action filter narrows it.
func TestAdminSecurityTab(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	if rec := s.Do(t, "GET", "/-/admin/security-log", nil); rec.Code != http.StatusForbidden {
		t.Errorf("owner opens the tab = %d, want 403", rec.Code)
	}
	if err := s.Orch.LoginRefused(ctx, "dev@smoke.dev", service.RequestMeta{IP: "203.0.113.9", UserAgent: "curl"}); err != nil {
		t.Fatal(err)
	}
	root := s.Session(t, s.User(t, "root@x.test", true))
	rec := s.As(t, root, "GET", "/-/admin/security-log", nil)
	body := rec.Body.String()
	for _, want := range []string{`id="tab-security"`, "sign-in rate limited", "dev@smoke.dev", "203.0.113.9", "refused"} {
		if rec.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("tab = %d, no %q in\n%s", rec.Code, want, body)
		}
	}
	body = s.As(t, root, "GET", "/-/admin/security-log?action=key.mint", nil).Body.String()
	if strings.Contains(body, "203.0.113.9") || !strings.Contains(body, "Nothing matches") {
		t.Errorf("action filter kept the other rows:\n%s", body)
	}
	// a target that is gone reads as such, never as its id
	if got := s.Orch.ScopePath(ctx, service.ScopeEnv, "00000000-0000-0000-0000-000000000000"); got != "a removed env" {
		t.Errorf("a removed env reads %q", got)
	}
}

// The log words every action in plain words: the listed ones by label, any
// other with no dotted key left in it.
func TestSecurityActionWords(t *testing.T) {
	for in, want := range map[string]string{
		"placement.pin":         "placement pinned",
		"move.finish":           "env move finished",
		"cert.controller_renew": "controller certificate renewed",
		"some.new_thing":        "some new thing",
	} {
		if got := admin.ActionWord(in); got != want {
			t.Errorf("%s = %q, want %q", in, got, want)
		}
	}
}

// Keys shows the Master key section; a wrong password is refused
// and nothing is written.
func TestAdminMasterKeyRotateWrongPassword(t *testing.T) {
	s := webtest.New(t)
	uid := s.User(t, "root@x.test", true)
	if err := s.Orch.SetPassword(context.Background(), uid, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	root := s.Session(t, uid)
	if body := s.As(t, root, "GET", "/-/admin/keys", nil).Body.String(); !strings.Contains(body, `id="master-key"`) || !strings.Contains(body, "Master key") {
		t.Fatalf("no Master key section in\n%s", body)
	}
	rec := s.As(t, root, "POST", "/-/admin/security/master-key/rotate", url.Values{"password": {"wrong"}})
	if rec.Code != http.StatusUnprocessableEntity || strings.Contains(rec.Body.String(), "restarting") {
		t.Errorf("wrong password = %d\n%s", rec.Code, rec.Body.String())
	}
}
