// Package auth_test covers the signed-out and first-run pages together:
// login's way back, setup, the invite link and the CLI approval.
package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

func owner(t *testing.T, s *webtest.Site) string {
	t.Helper()
	us, err := s.O.Users(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range us {
		if u.Email == "owner@acme.test" {
			return u.ID
		}
	}
	t.Fatal("no owner")
	return ""
}

// A visitor's page load goes to log in and names the way back; htmx and
// streams keep their 401; the way back is only ever a path on this site.
func TestLoginFirst(t *testing.T) {
	s := webtest.New(t)
	for path, want := range map[string]string{
		"/acme/shop?drawer=x": "/login?next=%2Facme%2Fshop%3Fdrawer%3Dx",
		"/account":            "/login?next=%2Faccount",
		"/":                   "/login",
	} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
			t.Errorf("visitor %s = %d %q, want 303 %q", path, rec.Code, rec.Header().Get("Location"), want)
		}
	}
	if rec := s.As(t, "", "GET", "/acme/shop", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("htmx visitor = %d, want 401", rec.Code)
	}
	for in, want := range map[string]string{"/acme": "/acme", "//evil.test": "", "/\\evil.test": "", "/\t/evil.test": "", "https://evil.test": "", "": ""} {
		if got := middleware.SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
	if body := s.As(t, "", "GET", "/login?next=//evil.test", nil).Body.String(); strings.Contains(body, "evil.test") {
		t.Error("the login page kept a way back off the site")
	}
}

// Setup: an admin with no org gets the create form; a member of one is
// sent to it; anyone else is told to ask for an invite.
func TestSetup(t *testing.T) {
	s := webtest.New(t)
	if rec := s.Do(t, "GET", "/setup", nil); rec.Header().Get("HX-Redirect") != "/acme" {
		t.Errorf("member setup = %d %v", rec.Code, rec.Header())
	}
	admin := s.Session(t, s.User(t, "root@x.test", true))
	if body := s.As(t, admin, "GET", "/setup", nil).Body.String(); !strings.Contains(body, `hx-post="/-/new-org"`) {
		t.Errorf("admin setup has no create form:\n%s", body)
	}
	rec := s.As(t, admin, "POST", "/-/new-org", url.Values{"name": {"First"}})
	if rec.Header().Get("HX-Redirect") != "/first" {
		t.Errorf("create first org = %d %v", rec.Code, rec.Header())
	}
	lone := s.Session(t, s.User(t, "lone@x.test", false))
	if body := s.As(t, lone, "GET", "/setup", nil).Body.String(); !strings.Contains(body, "invite link") || strings.Contains(body, "new-org") {
		t.Errorf("non-admin setup:\n%s", body)
	}
}

// An invite link: a visitor registers through it and lands on the org; a
// used link says so; a signed-in user accepts with one button.
func TestInvite(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	inv, err := s.O.Invite(ctx, s.Org, "new@x.test", "owner", owner(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if body := s.As(t, "", "GET", "/invite/"+inv.ID, nil).Body.String(); !strings.Contains(body, "new@x.test") || !strings.Contains(body, "/invite/"+inv.ID+"/register") {
		t.Fatalf("visitor invite page:\n%s", body)
	}
	rec := s.As(t, "", "POST", "/invite/"+inv.ID+"/register", url.Values{"name": {"New"}, "email": {"new@x.test"}, "password": {"Str0ng!pass"}})
	if rec.Header().Get("HX-Redirect") != "/acme" {
		t.Fatalf("register through invite = %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if rec := s.As(t, "", "GET", "/invite/"+inv.ID, nil); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "used or expired") {
		t.Errorf("used invite = %d", rec.Code)
	}

	other := s.User(t, "other@x.test", false)
	inv, err = s.O.Invite(ctx, s.Org, "other@x.test", "owner", owner(t, s))
	if err != nil {
		t.Fatal(err)
	}
	sess := s.Session(t, other)
	if body := s.As(t, sess, "GET", "/invite/"+inv.ID, nil).Body.String(); !strings.Contains(body, `hx-post="/invite/`+inv.ID+`"`) {
		t.Fatalf("signed-in invite page has no accept:\n%s", body)
	}
	if rec := s.As(t, sess, "POST", "/invite/"+inv.ID, nil); rec.Header().Get("HX-Redirect") != "/acme" {
		t.Errorf("accept = %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
}

// The CLI approval sends a code to the loopback listener only, and the
// code works once.
func TestCLIAuthorize(t *testing.T) {
	s := webtest.New(t)
	body := s.Do(t, "GET", "/cli/authorize?port=4711&state=abc&name=laptop", nil).Body.String()
	if !strings.Contains(body, `hx-post="/cli/authorize/acme"`) || !strings.Contains(body, "laptop") {
		t.Fatalf("authorize page:\n%s", body)
	}
	if rec := s.Do(t, "GET", "/cli/authorize?port=evil.test&state=abc", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad port page = %d", rec.Code)
	}
	if rec := s.Do(t, "POST", "/cli/authorize/acme", url.Values{"port": {"80@evil.test"}, "state": {"abc"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("bad port approve = %d", rec.Code)
	}
	rec := s.Do(t, "POST", "/cli/authorize/acme", url.Values{"port": {"4711"}, "state": {"a&b"}, "name": {"laptop"}})
	to, err := url.Parse(rec.Header().Get("HX-Redirect"))
	if err != nil || to.Host != "127.0.0.1:4711" || to.Query().Get("state") != "a&b" {
		t.Fatalf("approve = %d %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	if _, _, err := s.O.ExchangeCLICode(context.Background(), to.Query().Get("code")); err != nil {
		t.Errorf("the code does not exchange: %v", err)
	}
}
