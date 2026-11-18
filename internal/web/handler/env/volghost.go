package env

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
)

// The two Discard verbs of a ghost volume. Work package 2F fills them in;
// until then they refuse and the drawer shows why.

// POST …/-/volumes/:volume/discard-copy: remove the plain copy, keep the folder.
func (h *handler) DiscardCopy(c echo.Context) error {
	err := h.orch.DiscardPlainCopy(c.Request().Context(), middleware.Principal(c), c.Param("volume"), c.FormValue("confirm"))
	return h.volume(c, "overview", "", err)
}

// POST …/-/volumes/:volume/discard-object: remove an object bound elsewhere.
func (h *handler) DiscardObject(c echo.Context) error {
	err := h.orch.DiscardBoundObject(c.Request().Context(), middleware.Principal(c), c.Param("volume"), c.FormValue("confirm"))
	return h.volume(c, "overview", "", err)
}
