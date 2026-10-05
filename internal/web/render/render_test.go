package render_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
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
