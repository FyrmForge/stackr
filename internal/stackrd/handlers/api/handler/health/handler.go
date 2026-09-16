package health

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store   repo.Store
	version string
}

// NewHandler creates a new API health handler.
func NewHandler(store repo.Store, version string) *handler {
	return &handler{store: store, version: version}
}

// GET /api/health
//
// Carries the build version: the update page polls it to tell the new panel
// from the old task still answering.
func (h *handler) Health(c echo.Context) error {
	if err := h.store.Health(c.Request().Context()); err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"status":  "unhealthy",
			"error":   err.Error(),
			"version": h.version,
		})
	}
	return c.JSON(http.StatusOK, map[string]string{
		"status":  "healthy",
		"version": h.version,
	})
}
