package devgallery_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/server"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web"
)

// The gallery renders every shared component without error.
func TestGalleryRendersEveryComponent(t *testing.T) {
	env := servicetest.New(t)
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	web.RegisterRoutes(srv, &web.Deps{Orch: env.Orch, Access: middleware.NewAccess(env.Orch), DevMode: true})
	rec := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dev/components", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dev/components = %d\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for component, marker := range map[string]string{
		"layout":        `id="shell-header"`,
		"flash":         "<flash-toast",
		"theme toggle":  "<theme-toggle",
		"form field":    `id="error-email"`,
		"form error":    "Please fix the errors below.",
		"disabled why":  "Owned by stackr-compose.yml",
		"table":         "<table",
		"empty state":   "No tiles yet",
		"pagination":    "Page 2 of 5",
		"tile badge":    ">degraded<",
		"job badge":     ">superseded<",
		"env badge":     ">violet<",
		"tile card":     "nginx:1",
		"volume card":   "orphaned",
		"confirm":       "<confirm-dialog",
		"what is kept":  "archives already in the bucket",
		"log pane":      "<log-pane",
		"log line":      "listening on :80",
		"job status":    `sse-swap="update"`,
		"plan":          "staging does not run release 12 yet",
		"param editor":  `name="param.app.mode"`,
		"secret masked": `placeholder="unchanged"`,
		"settings form": "inherit (30)",
		"graph canvas":  "<graph-canvas",
		"graph node":    `<graph-node node-id="tile:api"`,
		"edge":          `data-from="tile:api" data-to="tile:db"`,
		"hidden x":      `name="x"`,
		"move trigger":  `hx-trigger="node-moved"`,
		"no inherit":    `hx-disinherit="*"`,
		"side drawer":   `id="drawer-body"`,
		"service card":  `data-kind="service"`,
		"cron card":     `data-kind="cron"`,
		"function card": `data-kind="function"`,
		"managed card":  `data-kind="managed"`,
		"slice card":    `data-kind="slice"`,
		"ref card":      `data-kind="ref"`,
		"volume card 2": `data-kind="volume"`,
		"proxy card":    `data-kind="proxy"`,
		"internet card": `data-kind="internet"`,
		"vars card":     `data-kind="vars"`,
		"secrets card":  `data-kind="secrets"`,
		"sub-tile":      `data-kind="instance"`,
		"host chip":     ">host<",
		"cron footer":   "next Jul 25 03:00",
		"lane":          `data-edge-kind="traffic"`,
		"lane rate":     "2.2 MB/s",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("%s: gallery lacks %q", component, marker)
		}
	}
}

// The fake canvas's drawer route answers with a tab body, not a page.
func TestGalleryDrawer(t *testing.T) {
	env := servicetest.New(t)
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	web.RegisterRoutes(srv, &web.Deps{Orch: env.Orch, Access: middleware.NewAccess(env.Orch), DevMode: true})
	rec := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dev/components/drawer?node=tile:api&tab=logs", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Sample logs tab") ||
		strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("GET drawer = %d\n%s", rec.Code, rec.Body)
	}
}

// Outside dev mode the gallery is not mounted.
func TestGalleryIsDevOnly(t *testing.T) {
	env := servicetest.New(t)
	srv, err := server.New()
	if err != nil {
		t.Fatal(err)
	}
	web.RegisterRoutes(srv, &web.Deps{Orch: env.Orch, Access: middleware.NewAccess(env.Orch)})
	rec := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dev/components", nil))
	if rec.Code == http.StatusOK {
		t.Fatal("GET /dev/components answered 200 outside dev mode")
	}
}
