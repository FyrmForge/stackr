package admin

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
)

// Forget disk in the node drawer. Work package 2F fills this in; until then
// the verb refuses and the drawer shows why.

// POST …/nodes/:node/disk/forget: forget the disk, or resume a forgetting row.
func (h *handler) diskForget(c echo.Context) error {
	_, err := h.orch.ForgetNodeDisk(c.Request().Context(), middleware.Principal(c),
		c.FormValue("disk"), c.FormValue("confirm"))
	return h.nodeAnswer(c, "overview", "", err)
}
