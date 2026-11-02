package health

import (
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

type handler struct {
	orch *service.Orchestrator
}

// NewHandler creates a new API health handler.
func NewHandler(orch *service.Orchestrator) *handler {
	return &handler{orch: orch}
}

// GET /api/health
func (h *handler) Health(c echo.Context) error {
	if err := h.orch.Ping(c.Request().Context()); err != nil {
		// Public: the error names the database path. The log has it.
		slog.Warn("health: ping failed", "err", err)
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"status": "unhealthy"})
	}
	// Docker down stays a 200: the panel is up, the installer and upgrade
	// gates wait on the status code, and the banner already says why.
	if h.orch.DockerDown(c.Request().Context()) {
		return c.JSON(http.StatusOK, map[string]string{
			"status": "degraded",
			"docker": "down",
		})
	}
	return c.JSON(http.StatusOK, map[string]string{
		"status": "healthy",
	})
}
