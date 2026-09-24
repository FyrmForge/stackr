package api

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/handler/health"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

// Deps holds the dependencies for API route registration.
type Deps struct {
	Service *service.Orchestrator
	Access  *middleware.Access // shared with the web router
}

// RegisterRoutes registers all API route handlers on the server.
func RegisterRoutes(srv *server.Server, deps *Deps) {
	api := srv.Echo().Group("/api")
	api.Use(middleware.Logging())

	healthHandler := health.NewHandler(deps.Service)
	api.GET("/health", healthHandler.Health)

	authed := api.Group("", deps.Access.Load())
	// ponytail: placeholder so the middleware is mounted and tested; the org
	// endpoints replace it.
	authed.GET("/:org", func(c echo.Context) error { return c.NoContent(http.StatusNoContent) },
		deps.Access.Require("org.read"))
}
