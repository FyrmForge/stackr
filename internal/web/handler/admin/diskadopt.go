package admin

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
)

// Adopt disk in the node drawer. The service does the work; the plan dialog
// and the Resume button are drawn once nodedisk_adopt.templ is built, until
// then the plan is read and not shown.

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
