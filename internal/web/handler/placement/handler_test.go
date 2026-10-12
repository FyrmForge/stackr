package placement_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/server"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web"
)

type browser struct {
	env     *servicetest.Env
	h       http.Handler
	session string
}

func newBrowser(t *testing.T, role string) browser {
	t.Helper()
	env := servicetest.New(t)
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	web.RegisterRoutes(srv, &web.Deps{Orch: env.Orch, Access: middleware.NewAccess(env.Orch), DevMode: true})
	org := env.Org(t, "acme")
	u := env.User(t, role+"@acme.test", role == "admin")
	if role != "admin" {
		env.Member(t, org, u, role)
	}
	return browser{env, srv.Echo(), env.Session(t, u)}
}

func (b browser) do(t *testing.T, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-CSRF-Token", "tok")
	req.AddCookie(&http.Cookie{Name: "csrf", Value: "tok"})
	req.AddCookie(&http.Cookie{Name: b.env.Orch.Sessions().CookieName(), Value: b.session})
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	return rec
}

// An owner sees and saves the org's placement; a viewer sees it read-only.
func TestOrgPlacement(t *testing.T) {
	b := newBrowser(t, "owner")
	rec := b.do(t, "GET", "/acme/-/placement", nil)
	body := rec.Body.String()
	for _, want := range []string{`id="placement"`, "Runs on", "Ingress", "Exits", `hx-post="/acme/-/placement/pin"`, "Inherit: out through the node the app runs on."} {
		if rec.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("missing %q in %d\n%s", want, rec.Code, body)
		}
	}
	// a node nobody opened to the org is refused, with the reason shown
	rec = b.do(t, "POST", "/acme/-/placement/pin", url.Values{"node": {"nope"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not open to this organization") {
		t.Errorf("pin on a closed node = %d\n%s", rec.Code, rec.Body.String())
	}
	if rec = b.do(t, "POST", "/acme/-/placement/exits/add", url.Values{"node": {"own"}}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "own node (no exit)") {
		t.Errorf("add own rung = %d\n%s", rec.Code, rec.Body.String())
	}

	v := newBrowser(t, "viewer")
	body = v.do(t, "GET", "/acme/-/placement", nil).Body.String()
	if !strings.Contains(body, "Runs on") || strings.Contains(body, "/placement/pin") {
		t.Errorf("viewer got a write form\n%s", body)
	}
	if rec = v.do(t, "POST", "/acme/-/placement/pin", url.Values{"node": {""}}); rec.Code == http.StatusOK {
		t.Error("a viewer saved a pin")
	}
}
