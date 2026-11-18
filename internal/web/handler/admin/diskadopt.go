package admin

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
)

// Adopt disk in the node drawer. Work package 2A fills these in; until then
// the verbs refuse and the drawer shows why.

// POST …/nodes/:node/disk/adopt/plan: what the adopt would do.
func (h *handler) diskAdoptPlan(c echo.Context) error {
	_, err := h.orch.PlanDiskAdopt(c.Request().Context(), c.Param("node"), c.FormValue("path"))
	return h.nodeAnswer(c, "overview", "", err)
}

// POST …/nodes/:node/disk/adopt: adopt, or resume an adopting row.
func (h *handler) diskAdopt(c echo.Context) error {
	_, err := h.orch.AdoptDisk(c.Request().Context(), middleware.Principal(c), c.Param("node"),
		c.FormValue("disk"), c.FormValue("path"), c.FormValue("confirm"))
	return h.nodeAnswer(c, "overview", "", err)
}
