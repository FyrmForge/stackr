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
	web.RegisterRoutes(srv, &web.Deps{Service: env.O, Access: middleware.NewAccess(env.O), DevMode: true})
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
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("%s: gallery lacks %q", component, marker)
		}
	}
}

// Outside dev mode the gallery is not mounted.
func TestGalleryIsDevOnly(t *testing.T) {
	env := servicetest.New(t)
	srv, err := server.New()
	if err != nil {
		t.Fatal(err)
	}
	web.RegisterRoutes(srv, &web.Deps{Service: env.O, Access: middleware.NewAccess(env.O)})
	rec := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dev/components", nil))
	if rec.Code == http.StatusOK {
		t.Fatal("GET /dev/components answered 200 outside dev mode")
	}
}
