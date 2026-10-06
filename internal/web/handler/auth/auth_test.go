// Package auth_test covers the signed-out pages together: login's way
// back, the invite link and the CLI approval. Setup is handler/setup's.
package auth_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

func owner(t *testing.T, s *webtest.Site) string {
	t.Helper()
	us, err := s.Orch.Users(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(us, func(u service.User) bool { return u.Email == "owner@acme.test" })
	if i < 0 {
		t.Fatal("no owner")
	}
	return us[i].ID
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
	for in, want := range map[string]string{
		"/acme":             "/acme",
		"//evil.test":       "",
		"/\\evil.test":      "",
		"/\t/evil.test":     "",
		"https://evil.test": "",
		"":                  "",
	} {
		if got := middleware.SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
	if body := s.As(t, "", "GET", "/login?next=//evil.test", nil).Body.String(); strings.Contains(body, "evil.test") {
		t.Error("the login page kept a way back off the site")
	}
}

// An invite link: a visitor registers through it and lands on the org; a
// used link says so; a signed-in user accepts with one button.
func TestInvite(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	inv, err := s.Orch.Invite(ctx, s.Org, "new@x.test", "owner", owner(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if body := s.As(t, "", "GET", "/invite/"+inv.ID, nil).Body.String(); !strings.Contains(body, "new@x.test") ||
		!strings.Contains(body, "/invite/"+inv.ID+"/register") {
		t.Fatalf("visitor invite page:\n%s", body)
	}
	rec := s.As(t, "", "POST", "/invite/"+inv.ID+"/register", url.Values{
		"name":             {"New"},
		"email":            {"new@x.test"},
		"password":         {"Str0ng!pass"},
		"confirm_password": {"Str0ng!pass"},
	})
	if rec.Header().Get("HX-Redirect") != "/acme" {
		t.Fatalf("register through invite = %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if rec := s.As(t, "", "GET", "/invite/"+inv.ID, nil); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), "no longer valid") {
		t.Errorf("used invite = %d", rec.Code)
	}

	other := s.User(t, "other@x.test", false)
	inv, err = s.Orch.Invite(ctx, s.Org, "other@x.test", "owner", owner(t, s))
	if err != nil {
		t.Fatal(err)
	}
	sess := s.Session(t, other)
	if body := s.As(
		t,
		sess,
		"GET",
		"/invite/"+inv.ID,
		nil,
	).Body.String(); !strings.Contains(body, `hx-post="/invite/`+inv.ID+`"`) {
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
	if rec := s.Do(t, "POST", "/cli/authorize/acme", url.Values{
		"port":  {"80@evil.test"},
		"state": {"abc"},
	}); rec.Code != http.StatusBadRequest {
		t.Errorf("bad port approve = %d", rec.Code)
	}
	rec := s.Do(t, "POST", "/cli/authorize/acme", url.Values{"port": {"4711"}, "state": {"a&b"}, "name": {"laptop"}})
	to, err := url.Parse(rec.Header().Get("HX-Redirect"))
	if err != nil || to.Host != "127.0.0.1:4711" || to.Query().Get("state") != "a&b" {
		t.Fatalf("approve = %d %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	if _, _, err := s.Orch.ExchangeCLICode(context.Background(), to.Query().Get("code")); err != nil {
		t.Errorf("the code does not exchange: %v", err)
	}
}

// No public sign-up: the installer makes the admin and invites bring the
// rest, so a visitor's POST /register makes no account.
func TestNoPublicRegister(t *testing.T) {
	s := webtest.New(t)
	before, err := s.Orch.Users(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rec := s.As(t, "", "POST", "/register", url.Values{
		"email": {"new@acme.test"}, "password": {"Longenough1!"}, "confirm_password": {"Longenough1!"}, "name": {"New"},
	})
	if rec.Code < 400 {
		t.Errorf("POST /register = %d, want a 4xx", rec.Code)
	}
	after, _ := s.Orch.Users(context.Background())
	if len(after) != len(before) {
		t.Errorf("an account was made: %d users, was %d", len(after), len(before))
	}
	if body := s.As(t, "", "GET", "/login", nil).Body.String(); strings.Contains(body, `href="/register`) {
		t.Error("the login page still links to /register")
	}
}

// A wrong password comes back as a 422 the page swaps in, error and all; a
// 401 is not swapped (components.htmxConfig) and the form said nothing.
func TestLoginWrongPassword(t *testing.T) {
	s := webtest.New(t)
	rec := s.As(t, "", "POST", "/login", url.Values{"email": {"nobody@acme.test"}, "password": {"not-the-password"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Invalid email or password") {
		t.Errorf("wrong password = %d %q, want 422 with the error", rec.Code, rec.Body.String())
	}
}

// An empty or malformed login says what to enter, in our words, not the
// browser's (the form is novalidate) or hamr's defaults.
func TestLoginFixFirstMessages(t *testing.T) {
	s := webtest.New(t)
	for _, c := range []struct{ email, want string }{
		{"", "Enter your email."},
		{"nope", "Enter an email like name@example.com."},
	} {
		rec := s.As(t, "", "POST", "/login", url.Values{"email": {c.email}, "password": {""}})
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), c.want) ||
			!strings.Contains(rec.Body.String(), "Enter your password.") {
			t.Errorf("email %q = %d %q, want %q", c.email, rec.Code, rec.Body.String(), c.want)
		}
	}
}

// Past ten tries on one email from one address, login answers 429 with the
// form and a time to come back; another email, or another address, goes on.
// Past a hundred tries from one address across emails, it is limited too.
// Only wrong passwords count.
func TestLoginRateLimit(t *testing.T) {
	s := webtest.New(t)
	try := func(ip, email string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{
			"email": {email}, "password": {"guess"},
		}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.Header.Set("X-CSRF-Token", "tok") // as webtest's send does
		req.AddCookie(&http.Cookie{Name: "csrf", Value: "tok"})
		req.RemoteAddr = ip + ":1234"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	for i := range 10 {
		if rec := try("203.0.113.7", "a@acme.test"); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("try %d limited already", i+1)
		}
	}
	rec := try("203.0.113.7", "A@acme.test ")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "Too many tries") ||
		rec.Header().Get("Retry-After") == "" {
		t.Errorf("11th try = %d %q, want 429 with the form and Retry-After", rec.Code, rec.Body.String())
	}
	if rec := try("203.0.113.7", "b@acme.test"); rec.Code == http.StatusTooManyRequests {
		t.Error("another email from the same address was limited")
	}
	if rec := try("198.51.100.9", "a@acme.test"); rec.Code == http.StatusTooManyRequests {
		t.Error("the same email from another address was limited")
	}
	// a right password gives its try back: signing in often never locks out
	if err := s.Orch.SetPassword(context.Background(), owner(t, s), "right-password-1"); err != nil {
		t.Fatal(err)
	}
	for i := range 15 {
		req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{
			"email": {"owner@acme.test"}, "password": {"right-password-1"},
		}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.Header.Set("X-CSRF-Token", "tok")
		req.AddCookie(&http.Cookie{Name: "csrf", Value: "tok"})
		req.RemoteAddr = "203.0.113.50:1234"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("good login %d limited", i+1)
		}
	}
	limited := false
	for i := range 101 {
		if try("192.0.2.1", fmt.Sprintf("u%d@acme.test", i)).Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Error("101 emails from one address were never limited")
	}
}
