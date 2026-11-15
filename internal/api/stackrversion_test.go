package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FyrmForge/hamr/pkg/server"

	"github.com/FyrmForge/stackr/internal/api"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Only a call past the auth gate carries the server's version.
func TestStackrVersionHeader(t *testing.T) {
	env := servicetest.NewWith(t, nil, func(c *service.Config) { c.Version = "v9.9.9" })
	srv, err := server.New(server.WithDevMode(true), server.WithGzipConfig(api.Gzip))
	if err != nil {
		t.Fatal(err)
	}
	api.RegisterRoutes(srv, &api.Deps{Orch: env.Orch, Access: middleware.NewAccess(env.Orch), CSRFCookie: "csrf"})
	admin := env.APIKey(t, env.User(t, "admin@x", true), "")
	for _, c := range []struct {
		name, key, header string
		code              int
	}{
		{"authed", admin, "v9.9.9", 200},
		{"no key", "", "", 401},
		{"bad key", "nope", "", 401},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/version", nil)
		if c.key != "" {
			req.Header.Set("Authorization", "Bearer "+c.key)
		}
		rec := httptest.NewRecorder()
		srv.Echo().ServeHTTP(rec, req)
		if rec.Code != c.code || rec.Header().Get("Stackr-Version") != c.header {
			t.Errorf("%s: %d %q, want %d %q", c.name, rec.Code, rec.Header().Get("Stackr-Version"), c.code, c.header)
		}
	}
}
