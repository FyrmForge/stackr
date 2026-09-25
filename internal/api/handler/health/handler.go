package health

import (
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
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"status": "unhealthy",
			"error":  err.Error(),
		})
	}
	return c.JSON(http.StatusOK, map[string]string{
		"status": "healthy",
	})
}
