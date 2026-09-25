package account_test

import (
	"context"
	"net/http"
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
