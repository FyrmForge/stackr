package web_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/server"

	"github.com/FyrmForge/stackr/internal/api"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web"
)

// An org's home is /<slug>, so a slug equal to the first segment of any panel
// or API route would lose its URL. Every registered first segment must be
// refused as an org name; a new route fails here until slug.RouteReserved
// lists it.
func TestRouteSegmentsAreReservedOrgSlugs(t *testing.T) {
	env := servicetest.New(t)
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	acc := middleware.NewAccess(env.Orch)
	web.RegisterRoutes(srv, &web.Deps{Orch: env.Orch, Access: acc, DevMode: true})
	api.RegisterRoutes(srv, &api.Deps{Orch: env.Orch, Access: acc, DevMode: true})
	org := env.Org(t, "acme")
	segs := map[string]bool{"static": true}
	for _, r := range srv.Echo().Routes() {
		seg, _, _ := strings.Cut(strings.TrimPrefix(r.Path, "/"), "/")
		if seg != "" && !strings.HasPrefix(seg, ":") && !strings.HasPrefix(seg, "*") {
			segs[seg] = true
		}
	}
	for seg := range segs {
		if _, err := env.Orch.RenameOrg(context.Background(), org, seg); err == nil {
			t.Errorf("org named %q accepted, but /%s is a route", seg, seg)
		}
	}
}
