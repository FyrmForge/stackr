package api

import (
	"github.com/FyrmForge/hamr/pkg/server"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/repo"
	"github.com/FyrmForge/stackr/internal/api/handler/health"
)

// Deps holds the dependencies for API route registration.
type Deps struct {
	Store repo.Store
}

// RegisterRoutes registers all API route handlers on the server.
func RegisterRoutes(srv *server.Server, deps *Deps) {
	api := srv.Echo().Group("/api")
	api.Use(middleware.Logging())

	healthHandler := health.NewHandler(deps.Store)
	api.GET("/health", healthHandler.Health)
}
