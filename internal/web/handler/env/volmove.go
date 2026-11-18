package env

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
)

// Move to disk in the volume drawer.

// POST …/-/volumes/:volume/move
func (h *handler) MoveToDisk(c echo.Context) error {
	_, err := h.orch.MoveVolumeToDisk(c.Request().Context(), middleware.Principal(c), c.Param("volume"))
	return h.volume(c, "overview", "moving to the disk", err)
}
