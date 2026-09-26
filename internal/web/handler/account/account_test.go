package account_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The account page changes the password and revokes a key.
func TestAccount(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	u := s.User(t, "me@x.test", false)
	s.Member(t, s.Org, u, "owner")
	sess := s.Session(t, u)
	if err := s.Orch.SetPassword(ctx, u, "0ld!Password"); err != nil {
		t.Fatal(err)
	}
	_, k, err := s.Orch.MintKey(ctx, u, s.Org, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if body := s.As(t, sess, "GET", "/account?tab=keys", nil).Body.String(); !strings.Contains(body, "laptop") ||
		!strings.Contains(body, "/account/keys/"+k.ID+"/revoke") {
		t.Fatalf("account page:\n%s", body)
	}
	if rec := s.As(t, sess, "POST", "/account/password", url.Values{
		"current_password": {"wrong"},
		"password":         {"N3w!password"},
		"confirm_password": {"N3w!password"},
	}); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "current password is wrong") {
		t.Errorf("wrong current = %d %s", rec.Code, rec.Body)
	}
	if rec := s.As(t, sess, "POST", "/account/password", url.Values{
		"current_password": {"0ld!Password"},
		"password":         {"N3w!password"},
		"confirm_password": {"N3w!passwor"},
	}); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "do not match") {
		t.Errorf("mismatch = %d %s", rec.Code, rec.Body)
	}
	if rec := s.As(t, sess, "POST", "/account/password", url.Values{
		"current_password": {"0ld!Password"},
		"password":         {"N3w!password"},
		"confirm_password": {"N3w!password"},
	}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Password changed") {
		t.Errorf("change = %d %s", rec.Code, rec.Body)
	}
	rec := s.As(t, sess, "POST", "/account/keys/"+k.ID+"/revoke", nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "laptop") {
		t.Errorf("revoke = %d %s", rec.Code, rec.Body)
	}
}

// The theme is per user and v0-exact: three words only, saved with a
// reload, written as the <html> class on every page, the error page
// included; system and signed out write none, so the OS decides.
func TestAppearance(t *testing.T) {
	s := webtest.New(t)
	u := s.User(t, "me@x.test", false)
	sess := s.Session(t, u)
	full := func(session, path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if session != "" {
			req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: session})
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if body := full(sess, "/account?tab=appearance"); !strings.Contains(body, `<html lang="en">`) ||
		!strings.Contains(body, `value="system" checked`) {
		t.Fatalf("system theme page:\n%s", body)
	}
	if rec := s.As(t, sess, "POST", "/account/appearance", url.Values{"theme": {"blue"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("blue = %d, want 400", rec.Code)
	}
	rec := s.As(t, sess, "POST", "/account/appearance", url.Values{"theme": {"light"}})
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "/account?tab=appearance" {
		t.Fatalf("light = %d %v", rec.Code, rec.Header())
	}
	if body := full(sess, "/account?tab=appearance"); !strings.Contains(body, `<html lang="en" class="light">`) ||
		!strings.Contains(body, `value="light" checked`) {
		t.Errorf("light theme page:\n%s", body)
	}
	if body := full(sess, "/no-such-org"); !strings.Contains(body, `<html lang="en" class="light">`) {
		t.Errorf("error page lost the theme:\n%s", body)
	}
	if body := full("", "/login"); !strings.Contains(body, `<html lang="en">`) {
		t.Errorf("signed out page has a theme class:\n%s", body)
	}
}
