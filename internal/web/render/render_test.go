package render_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// One route, three ways: a plain GET gets the layout, an htmx navigation
// gets the #main fragment with the header out of band, a history restore
// gets the layout again.
func TestPageRendersBothWays(t *testing.T) {
	env := servicetest.New(t)
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	web.RegisterRoutes(srv, &web.Deps{Orch: env.Orch, Access: middleware.NewAccess(env.Orch), DevMode: true})
	acme := env.Org(t, "acme")
	owner := env.User(t, "owner@x", false)
	env.Member(t, acme, owner, "owner")
	env.Tile(t, acme)
	session := env.Session(t, owner)

	get := func(headers map[string]string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/acme/shop/dev/api", nil)
		req.AddCookie(&http.Cookie{Name: env.Orch.Sessions().CookieName(), Value: session})
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		srv.Echo().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	for _, tt := range []struct {
		name    string
		headers map[string]string
		full    bool
	}{
		{"plain", nil, true},
		{"htmx", map[string]string{"HX-Request": "true"}, false},
		{"history restore", map[string]string{"HX-Request": "true", "HX-History-Restore-Request": "true"}, true},
	} {
		code, body := get(tt.headers)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d", tt.name, code)
		}
		for _, want := range []string{
			"Nothing here yet.",
			`id="shell-rail"`, // the signed-in shell: v0's rail shows no email
			`href="/acme/shop/dev"`,
			`aria-current="page"`,
			"<title>Overview - stackr</title>",
			`hx-get="/acme/shop/dev/-/tiles/api?tab=settings" hx-target="#drawer-body" hx-push-url="/acme/shop/dev?drawer=`, // a tile page has its Settings button, pushed as the env canvas with the drawer
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: body lacks %q", tt.name, want)
			}
		}
		if got := strings.Contains(body, "<html"); got != tt.full {
			t.Errorf("%s: full page = %v, want %v", tt.name, got, tt.full)
		}
		if got := strings.Contains(body, `hx-swap-oob="true"`); got == tt.full {
			t.Errorf("%s: out-of-band header = %v, want %v", tt.name, got, !tt.full)
		}
	}
}

// The phone tab select asks for Base?tab=&drawer=node and gets the same
// pushed URL the tab links carry; a request without the node pushes none.
func TestDrawerTabPushesURL(t *testing.T) {
	e := echo.New()
	h := render.DrawerTab(func(c echo.Context) error { return c.NoContent(http.StatusOK) })
	for query, want := range map[string]string{
		"tab=vars&drawer=org:1": "?drawer=org%3A1&tab=vars",
		"tab=vars":              "",
	} {
		req := httptest.NewRequest(http.MethodGet, "/-/drawer?"+query, nil)
		req.Header.Set("HX-Target", "drawer-view")
		rec := httptest.NewRecorder()
		if err := h(e.NewContext(req, rec)); err != nil {
			t.Fatal(err)
		}
		if got := rec.Header().Get("HX-Push-Url"); got != want {
			t.Errorf("%s: HX-Push-Url = %q, want %q", query, got, want)
		}
	}
}

func TestParamEntries(t *testing.T) {
	got := render.ParamEntries(map[string][]string{
		"secret.s3.key": {""}, "param.s3.region": {"eu"}, "junk": {"x"}, "param.bad": {"x"},
		"new_collection": {" s3 "}, "new_name": {" bucket "}, "new_kind": {"param"}, "new_value": {"b"},
	})
	want := []service.ParamEntry{
		{Collection: "s3", Name: "key", Kind: "secret"},
		{Collection: "s3", Name: "region", Kind: "param", Value: "eu"},
		{Collection: "s3", Name: "bucket", Kind: "param", Value: "b"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("entries = %+v, want %+v", got, want)
	}
	if got := render.ParamEntries(map[string][]string{"new_name": {" "}}); len(got) != 0 {
		t.Errorf("a blank new row = %+v", got)
	}
}

// An offline node is a refusal the user acts on (start it), shown inline,
// not the error page; a plain 503 stays a failure.
func TestRefusedNodeDown(t *testing.T) {
	err := fmt.Errorf("deploy: %w", &service.NodeDownError{Name: "vm106"})
	if msg, ok := render.Refused(err); !ok || msg != middleware.NodeOfflineMsg {
		t.Errorf("node down = %q %v", msg, ok)
	}
	if _, ok := render.Refused(echo.NewHTTPError(http.StatusServiceUnavailable, "x")); ok {
		t.Error("a plain 503 read as a refusal")
	}
}

// The problem banner is the admin's: an offline worker shows on the home
// page (no crumbs) for a server admin and never for a member.
func TestProblemsAdminOnly(t *testing.T) {
	env := servicetest.New(t)
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	web.RegisterRoutes(srv, &web.Deps{Orch: env.Orch, Access: middleware.NewAccess(env.Orch), DevMode: true})
	ctx := context.Background()
	n, err := env.Store.Nodes.Get(ctx, env.Worker(t, "h3", "x"))
	if err != nil {
		t.Fatal(err)
	}
	n.Status = "offline"
	if err := env.Store.Nodes.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.Settings.Set(ctx, "server_setup_done_at", "2026-10-01T00:00:00Z"); err != nil { // no setup redirect
		t.Fatal(err)
	}
	acme := env.Org(t, "acme")
	member := env.User(t, "member@x", false)
	env.Member(t, acme, member, "owner")
	admin := env.User(t, "admin@x", true)
	get := func(user string) string {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: env.Orch.Sessions().CookieName(), Value: env.Session(t, user)})
		rec := httptest.NewRecorder()
		srv.Echo().ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if body := get(admin); !strings.Contains(body, `id="problems" role="alert"`) || !strings.Contains(body, "h3 is offline") ||
		!strings.Contains(body, `href="?drawer=node:`+n.ID+`"`) {
		t.Errorf("admin: no banner:\n%s", body)
	}
	if body := get(member); strings.Contains(body, `id="problems"`) || strings.Contains(body, "is offline") {
		t.Error("a member saw the problem banner")
	}
}
