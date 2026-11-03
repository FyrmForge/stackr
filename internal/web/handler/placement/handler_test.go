package placement_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/server"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
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
	for _, want := range []string{`id="placement"`, "Runs on", "Ingress", "Exits", `hx-post="/acme/-/placement/pin"`, "Inherit: out through the placement the app runs on."} {
		if rec.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("missing %q in %d\n%s", want, rec.Code, body)
		}
	}
	// a node nobody opened to the org is refused, with the reason shown
	rec = b.do(t, "POST", "/acme/-/placement/pin", url.Values{"placement": {"nope"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not open to this organization") {
		t.Errorf("pin on a closed node = %d\n%s", rec.Code, rec.Body.String())
	}
	if rec = b.do(t, "POST", "/acme/-/placement/exits/add", url.Values{"placement": {"own"}}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `name="placement" value="own"`) {
		t.Errorf("add own rung = %d\n%s", rec.Code, rec.Body.String())
	}

	v := newBrowser(t, "viewer")
	body = v.do(t, "GET", "/acme/-/placement", nil).Body.String()
	if !strings.Contains(body, "Runs on") || strings.Contains(body, "/placement/pin") {
		t.Errorf("viewer got a write form\n%s", body)
	}
	if rec = v.do(t, "POST", "/acme/-/placement/pin", url.Values{"placement": {""}}); rec.Code == http.StatusOK {
		t.Error("a viewer saved a pin")
	}
}

// A node nobody opened to the org never shows in its tab, at any level, and
// the old move routes are gone (a pin change goes through the pin).
func TestNoNodeNames(t *testing.T) {
	b := newBrowser(t, "owner")
	b.env.Worker(t, "hidden-h4", "iss")
	for _, p := range []string{"/acme/-/placement"} {
		rec := b.do(t, "GET", p, nil)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "hidden-h4") {
			t.Errorf("GET %s = %d, names a node the org may not use\n%s", p, rec.Code, rec.Body)
		}
	}
	if rec := b.do(t, "POST", "/acme/-/placement/move-all", url.Values{}); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("move-all = %d, want it gone", rec.Code)
	}
}

// A stack pin that puts an env on another node answers with the move dialog:
// the env's line, its checks, and a confirm typed with the stack's slug that
// posts back to the pin route.
func TestStackPinMoveDialog(t *testing.T) {
	b := newBrowser(t, "owner")
	og, err := b.env.Store.Orgs.GetBySlug(t.Context(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	b.env.Tile(t, og.ID)
	b.env.Worker(t, "w9", "iss")
	adm := &service.Principal{Access: authz.User{ID: "a", Active: true, Admin: true}}
	if _, err := b.env.Orch.CreatePlacement(t.Context(), adm, service.PlacementSpec{Name: "far", Node: "w9", OpenToAll: true}); err != nil {
		t.Fatal(err)
	}
	rec := b.do(t, "POST", "/acme/shop/-/placement/pin", url.Values{"placement": {"far"}})
	body := rec.Body.String()
	for _, want := range []string{"acme/shop/dev", "Moves from default to far", "/acme/shop/-/placement/pin", `word="shop"`, "Move shop to far?"} {
		if !strings.Contains(body, want) {
			t.Errorf("dialog lacks %q (%d)\n%s", want, rec.Code, body)
		}
	}
	if st, _ := b.env.Store.Stacks.GetBySlug(t.Context(), og.ID, "shop"); st.PlacementID != nil {
		t.Error("the pin was written before the move was confirmed")
	}
}

// A member deploys but never moves an env: the tab is read-only and every
// placement write is refused, at each scope, as on the API.
func TestMemberCannotPin(t *testing.T) {
	m := newBrowser(t, "member")
	og, err := m.env.Store.Orgs.GetBySlug(t.Context(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	m.env.Tile(t, og.ID)
	body := m.do(t, "GET", "/acme/shop/-/placement", nil).Body.String()
	if !strings.Contains(body, "Runs on") || strings.Contains(body, "/placement/pin") {
		t.Errorf("member got a write form\n%s", body)
	}
	for _, p := range []string{"/acme/-/placement/pin", "/acme/shop/-/placement/pin", "/acme/shop/dev/-/placement/pin", "/acme/shop/dev/-/placement/move", "/acme/shop/dev/-/placement/ingress"} {
		if rec := m.do(t, "POST", p, url.Values{"placement": {"default"}}); rec.Code != http.StatusForbidden {
			t.Errorf("member POST %s = %d, want 403", p, rec.Code)
		}
	}
}
