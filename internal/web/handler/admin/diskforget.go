package admin

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
)

// Forget disk in the node drawer. The dialog (nodedisk_forget.templ) is part
// of the drawer, built from rows alone; this is the press. A refusal that was
// known on open never gets here: the dialog has no button then.

// POST …/nodes/:node/disk/forget: forget the disk, or resume a forgetting row.
// The record of a forgotten node's disk goes at once (no job).
func (h *handler) diskForget(c echo.Context) error {
	j, err := h.orch.ForgetNodeDisk(c.Request().Context(), middleware.Principal(c),
		c.FormValue("disk"), c.FormValue("confirm"))
	if err != nil {
		return h.nodeAnswer(c, "overview", "", err)
	}
	if j.ID == "" {
		return h.nodeAnswer(c, "overview", "Disk record forgotten.", nil)
	}
	c.Set("job", j)
	return h.nodeAnswer(c, "overview", "Forgetting the disk.", nil)
}
