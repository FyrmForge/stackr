package admin

import (
	"cmp"
	"net/http"
	"strconv"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// Move all to disk in the node drawer. The plan dialog is an explicit POST (it
// measures the volumes); a press that is refused draws the plan again with the
// reason, a press that queues jobs answers the drawer, whose live region then
// shows each job (moveJobs).

// moveAllDialog is the plan as the dialog shows it. msg is a refusal; a plan
// that is itself refused is only that message.
func (h *handler) moveAllDialog(c echo.Context, msg string) (ui.MoveAllDialog, error) {
	base := nodesBase + "/" + c.Param("node")
	m := ui.MoveAllDialog{Plan: base + "/disk/move-all/plan", Move: base + "/disk/move-all", Msg: msg}
	if n, err := h.orch.NodeDetail(c.Request().Context(), c.Param("node")); err == nil {
		m.Node = n.Name
	}
	p, err := h.orch.PlanMoveAllToDisk(c.Request().Context(), middleware.Principal(c), c.Param("node"))
	if err != nil {
		text, ok := render.Refused(err)
		if !ok {
			return m, err
		}
		m.Msg = cmp.Or(msg, text)
		return m, nil
	}
	m.Need, m.Free, m.Low = diskSize(p.Need), diskSize(int64(p.Free)), !p.Fits
	for i, g := range p.Groups {
		grp := ui.MoveGroup{Title: "Group " + strconv.Itoa(i+1)}
		if len(g.Tiles) > 0 {
			grp.Title += " · restarts " + strings.Join(g.Tiles, ", ")
		}
		for _, v := range g.Volumes {
			grp.Rows = append(grp.Rows, ui.MoveRow{Name: v.Slug, Where: v.Env, Note: moveNote(v)})
		}
		m.Count += len(g.Volumes)
		m.Groups = append(m.Groups, grp)
	}
	for _, v := range p.Skipped {
		m.Skipped = append(m.Skipped, ui.MoveRow{Name: v.Slug, Where: v.Env, Note: skipText(v.Reason)})
	}
	m.Count += len(p.Skipped)
	if p.Busy && m.Msg == "" {
		m.Msg = "Volumes are already moving to the disk."
	}
	return m, nil
}

// moveNote is what happens to a volume of a group: its size and the verb.
func moveNote(v service.MoveAllVolume) string {
	size := "size unknown"
	if v.SizeBytes >= 0 {
		size = diskSize(v.SizeBytes)
	}
	if v.Finishes {
		return size + " · finishes an earlier move"
	}
	return size + " · will move"
}

// skipText words a skipped volume's reason as the dialog does.
func skipText(r string) string {
	if r == "It does not fit on the disk." || r == "It does not fit on the disk" {
		return "Does not fit on the disk."
	}
	return r
}

// POST …/nodes/:node/disk/move-all/plan: what Move all would do.
func (h *handler) diskMoveAllPlan(c echo.Context) error {
	m, err := h.moveAllDialog(c, "")
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, http.StatusOK, ui.MoveAllDialogView(m))
}

// POST …/nodes/:node/disk/move-all: one volume-to-disk job per group.
func (h *handler) diskMoveAll(c echo.Context) error {
	js, err := h.orch.MoveAllToDisk(c.Request().Context(), middleware.Principal(c), c.Param("node"))
	if err == nil || len(js) > 0 {
		return h.nodeAnswer(c, "overview", "Moving the volumes to the disk.", err)
	}
	msg, ok := render.Refused(err)
	if !ok {
		return middleware.HTTPError(err)
	}
	m, err := h.moveAllDialog(c, msg)
	if err != nil {
		return middleware.HTTPError(err)
	}
	c.Response().Header().Set("HX-Retarget", "#moveall-dlg")
	c.Response().Header().Set("HX-Reswap", "innerHTML")
	return respond.HTML(c, http.StatusOK, ui.MoveAllDialogView(m))
}
