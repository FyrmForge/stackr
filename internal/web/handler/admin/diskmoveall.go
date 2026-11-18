package admin

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
)

// Move all to disk in the node drawer. Work package 2N fills these in; until
// then the verbs refuse and the drawer shows why.

// POST …/nodes/:node/disk/move-all/plan: what Move all would do.
func (h *handler) diskMoveAllPlan(c echo.Context) error {
	_, err := h.orch.PlanMoveAllToDisk(c.Request().Context(), middleware.Principal(c), c.Param("node"))
	return h.nodeAnswer(c, "overview", "", err)
}

// POST …/nodes/:node/disk/move-all: one volume-to-disk job per group.
func (h *handler) diskMoveAll(c echo.Context) error {
	_, err := h.orch.MoveAllToDisk(c.Request().Context(), middleware.Principal(c), c.Param("node"))
	return h.nodeAnswer(c, "overview", "", err)
}
