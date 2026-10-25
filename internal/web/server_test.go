package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/server"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The panel's checkboxes draw their tick from a data: SVG background in
// output.css, so the policy has to allow data: images or the tick is blocked.
func TestCSPAllowsDataImages(t *testing.T) {
	env := servicetest.New(t)
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	web.RegisterRoutes(srv, &web.Deps{Orch: env.Orch, Access: middleware.NewAccess(env.Orch), DevMode: true})
	rec := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self' data:") {
		t.Errorf("CSP = %q, want img-src 'self' data:", csp)
	}
}

// Cookies are Secure for an https BASE_URL whoever terminates TLS, and only
// for that. The session cookie takes the same helper; CSRF is checked on the wire.
func TestCookiesSecureFollowBaseURL(t *testing.T) {
	for _, c := range []struct {
		base   string
		secure bool
	}{{"https://panel.example.com", true}, {"http://localhost:8080", false}, {"", true}, {"::not a url", true}} {
		if got := web.CookieSecure(c.base); got != c.secure {
			t.Errorf("CookieSecure(%q) = %v, want %v", c.base, got, c.secure)
		}
		env := servicetest.New(t)
		srv, err := server.New()
		if err != nil {
			t.Fatal(err)
		}
		web.RegisterRoutes(srv, &web.Deps{Orch: env.Orch, Access: middleware.NewAccess(env.Orch), BaseURL: c.base})
		rec := httptest.NewRecorder()
		srv.Echo().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
		seen := false
		for _, ck := range rec.Result().Cookies() {
			seen = true
			if ck.Secure != c.secure {
				t.Errorf("base %q: cookie %s Secure = %v, want %v", c.base, ck.Name, ck.Secure, c.secure)
			}
		}
		if !seen {
			t.Errorf("base %q: no cookie set on /login", c.base)
		}
	}
}

// The web is session only: a viewer's API key on a web POST is a 401, so the
// key mint and the CLI login approval cannot be reached with one.
func TestWebRefusesKeys(t *testing.T) {
	s := webtest.New(t)
	u := s.User(t, "viewer@acme.test", false)
	s.Member(t, s.Org, u, "viewer")
	key := s.APIKey(t, u, s.Org)
	for _, path := range []string{"/acme/-/drawer/keys", "/cli/authorize"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("name=x&port=1&state=s"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-CSRF-Token", "tok")
		req.AddCookie(&http.Cookie{Name: "csrf", Value: "tok"})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s with a key = %d, want 401", path, rec.Code)
		}
	}
}
