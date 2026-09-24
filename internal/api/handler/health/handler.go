package health

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/repo"
)

type handler struct {
	store repo.Store
}

// NewHandler creates a new API health handler.
func NewHandler(store repo.Store) *handler {
	return &handler{store: store}
}

// GET /api/health
func (h *handler) Health(c echo.Context) error {
	if err := h.store.Health(c.Request().Context()); err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"status": "unhealthy",
			"error":  err.Error(),
		})
	}
	return c.JSON(http.StatusOK, map[string]string{
		"status": "healthy",
	})
}
