package api

import (
	"strings"

	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/handler/health"
	"github.com/FyrmForge/stackr/internal/api/handler/v1"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

// Deps holds the dependencies for API route registration.
type Deps struct {
	Orch    *service.Orchestrator
	Access  *middleware.Access // shared with the web router
	DevMode bool               // the CSRF cookie is not Secure in dev
}

// RegisterRoutes registers all API route handlers on the server.
func RegisterRoutes(srv *server.Server, deps *Deps) {
	api := srv.Echo().Group("/api")
	api.Use(middleware.Logging())

	healthHandler := health.NewHandler(deps.Orch)
	api.GET("/health", healthHandler.Health)

	h := &v1.H{Orch: deps.Orch}
	srv.Echo().POST("/hooks/connectors/:connector", h.Hook, middleware.Logging(), middleware.JSONErrors())

	g := api.Group("/v1", middleware.JSONErrors(), deps.Access.Load(), middleware.APICSRF(!deps.DevMode))
	for _, r := range Routes(h) {
		g.Add(r.Method, r.Path, r.E.Handle, gate(deps.Access, r))
	}
}

func gate(a *middleware.Access, r Route) echo.MiddlewareFunc {
	switch r.Verb {
	case Public:
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	case Self:
		return a.Authed()
	case "":
		panic("api: route " + r.Method + " " + r.Path + " names no verb")
	}
	return a.Require(r.Verb)
}

// Gzip is the server's compression, off for the streams: a compressor may
// hold back bytes a follow must see now.
var Gzip = server.GzipConfig{Enabled: true, Skipper: func(c echo.Context) bool {
	p := c.Request().URL.Path
	return strings.HasSuffix(p, "/events") || strings.HasSuffix(p, "/logs/stream") || strings.HasSuffix(p, "/exec")
}}
