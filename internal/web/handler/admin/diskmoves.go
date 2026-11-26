package admin

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// The controller's two rows on the Node disk card: logs and metrics (observe)
// and the panel database. Each opens a plan dialog (an explicit POST that
// measures), a press queues one job and answers the drawer, whose job slot and
// live region follow it. A press the plan refuses is answered in the dialog.

const (
	observeDlg = "observe-dlg"
	paneldbDlg = "paneldb-dlg"
)

// diskMoves is the card's two rows. The panel database row needs pv.Applicable
// (installed panel, a ready whole disk); a worker passes none of this.
func diskMoves(base string, obsOn, obsBusy bool, pv service.PanelDBView) *ui.DiskMoves {
	obs := ui.DiskMoveRow{
		Name: "Logs and metrics", Button: "Move logs and metrics onto this disk", Busy: obsBusy,
		Plan: base + "/disk/move-observe/plan", Dlg: observeDlg,
	}
	if obsOn {
		obs.State, obs.Button, obs.Vals = "On this disk", "Move back to the system disk", `{"back":"1"}`
	}
	m := &ui.DiskMoves{Rows: []ui.DiskMoveRow{obs}}
	if !pv.Applicable {
		return m
	}
	db := ui.DiskMoveRow{
		Name: "Panel database", Button: "Move the panel database onto this disk", Busy: pv.Moving,
		Plan: base + "/disk/move-paneldb/plan", Dlg: paneldbDlg,
	}
	if pv.OnDisk {
		db.State, db.Button, db.Vals = "On this disk", "Move back to the system disk", `{"back":"1"}`
	}
	if pv.OldCopy {
		db.Old, db.Remove = "Old copy", base+"/disk/move-paneldb/remove-old"
		if !pv.OldCopyAt.IsZero() {
			db.Old = "Old copy, taken " + pv.OldCopyAt.Format("02-01")
		}
		db.OldHint = "Deletes the old copy on the system disk."
		if pv.OldCopyOnDisk {
			db.OldHint = "Deletes the old copy on the node disk."
		}
		db.OldHint += " This cannot be undone."
	}
	m.Rows = append(m.Rows, db)
	return m
}

// movesCard fills the card's rows: rows and the job table only, no helper.
func (h *handler) movesCard(c echo.Context, dk *ui.NodeDisk, d service.NodeDetail, base string) error {
	ctx := c.Request().Context()
	on, err := h.orch.ObserveOnDisk(ctx)
	if err != nil {
		return err
	}
	busy, err := h.jobActive(c, d.ID, "observe-move")
	if err != nil {
		return err
	}
	pv, err := h.orch.PanelDBState(ctx, d.ID)
	if err != nil {
		return err
	}
	dk.Moves = diskMoves(base, on, busy, pv)
	return nil
}

// nodeID is the node of the route as an id.
func (h *handler) nodeID(c echo.Context) (id, name string, err error) {
	d, err := h.orch.NodeDetail(c.Request().Context(), c.Param("node"))
	return d.ID, d.Name, err
}

func moveBack(c echo.Context) bool { return c.FormValue("back") == "1" }

// moveDialog is the shell of a plan dialog: titles, the POSTs and the way back.
func moveDialog(c echo.Context, name, what, kind string, back bool) ui.DiskMoveDialog {
	base := nodesBase + "/" + c.Param("node") + "/disk/" + kind
	m := ui.DiskMoveDialog{Title: "Move " + what + " onto this disk", Sub: name, Button: "Move to disk", Move: base, Dlg: map[string]string{"move-observe": observeDlg, "move-paneldb": paneldbDlg}[kind]}
	if back {
		m.Title, m.Button, m.Vals = "Move "+what+" back to the system disk", "Move back", `{"back":"1"}`
	}
	return m
}

// refusedDialog answers a refusal: the message, and the move stays off.
func refusedDialog(c echo.Context, m ui.DiskMoveDialog, err error) error {
	msg, ok := render.Refused(err)
	if !ok {
		return middleware.HTTPError(err)
	}
	m.Msg = msg
	return respond.HTML(c, http.StatusOK, ui.DiskMoveDialogView(m))
}

// POST …/nodes/:node/disk/move-observe/plan: what the move of logs and metrics does.
func (h *handler) diskMoveObservePlan(c echo.Context) error {
	id, name, err := h.nodeID(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	back := moveBack(c)
	m := moveDialog(c, name, "logs and metrics", "move-observe", back)
	m.Restarts = "Logs and metrics stop while the data copies, then start again."
	p, err := h.orch.PlanMoveObserve(c.Request().Context(), middleware.Principal(c), id, back)
	if err != nil {
		return refusedDialog(c, m, err)
	}
	where := "Free on the node disk"
	if back {
		where = "Free on the system disk"
	}
	m.Facts = []ui.MoveFact{
		{Label: "Size", Value: diskSize(p.SizeBytes)},
		{Label: where, Value: diskSize(int64(p.Free)), Low: !p.Fits},
	}
	switch {
	case p.Busy:
		m.Msg = "Volumes are moving to the disk. Wait for them."
	case !p.Fits:
		m.Msg = "Not enough room. Needs " + diskSize(p.Need) + ", " + diskSize(int64(p.Free)) + " free."
	}
	return respond.HTML(c, http.StatusOK, ui.DiskMoveDialogView(m))
}

// POST …/nodes/:node/disk/move-observe: queue the move.
func (h *handler) diskMoveObserve(c echo.Context) error {
	id, name, err := h.nodeID(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	back := moveBack(c)
	j, err := h.orch.MoveObserve(c.Request().Context(), middleware.Principal(c), id, back)
	if err != nil {
		return h.refusedPress(c, moveDialog(c, name, "logs and metrics", "move-observe", back), err)
	}
	c.Set("job", j)
	return h.nodeAnswer(c, "overview", "Moving logs and metrics.", nil)
}

// POST …/nodes/:node/disk/move-paneldb/plan: what the move of the panel database does.
func (h *handler) diskMovePanelPlan(c echo.Context) error {
	id, name, err := h.nodeID(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	back := moveBack(c)
	m := moveDialog(c, name, "the panel database", "move-paneldb", back)
	p, err := h.orch.PlanMovePanelDB(c.Request().Context(), middleware.Principal(c), id, back)
	if err != nil {
		return refusedDialog(c, m, err)
	}
	m.Restarts, m.Warning, m.Msg = p.Restarts, p.Warning, p.Refusal
	if p.SizeBytes > 0 {
		m.Facts = []ui.MoveFact{
			{Label: "Size", Value: diskSize(p.SizeBytes)},
			{Label: "Free on the node disk", Value: diskSize(int64(p.DiskFree)), Low: !back && uint64(p.Need) > p.DiskFree},
			{Label: "Free on the system disk", Value: diskSize(int64(p.SystemFree)), Low: uint64(p.Need) > p.SystemFree},
		}
	}
	return respond.HTML(c, http.StatusOK, ui.DiskMoveDialogView(m))
}

// POST …/nodes/:node/disk/move-paneldb: queue the move.
func (h *handler) diskMovePanel(c echo.Context) error {
	id, name, err := h.nodeID(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	back := moveBack(c)
	j, err := h.orch.MovePanelDB(c.Request().Context(), middleware.Principal(c), id, back)
	if err != nil {
		return h.refusedPress(c, moveDialog(c, name, "the panel database", "move-paneldb", back), err)
	}
	c.Set("job", j)
	return h.nodeAnswer(c, "overview", "Moving the panel database.", nil)
}

// refusedPress answers a refused press in its dialog, which the answer retargets.
func (h *handler) refusedPress(c echo.Context, m ui.DiskMoveDialog, err error) error {
	c.Response().Header().Set("HX-Retarget", "#"+m.Dlg)
	c.Response().Header().Set("HX-Reswap", "innerHTML")
	return refusedDialog(c, m, err)
}

// POST …/nodes/:node/disk/move-paneldb/remove-old: the only deleter of the old copy.
func (h *handler) diskRemoveOldCopy(c echo.Context) error {
	return h.nodeAct("overview", "Old copy removed.", func(c echo.Context) error {
		id, _, err := h.nodeID(c)
		if err != nil {
			return err
		}
		return h.orch.RemovePanelDBOldCopy(c.Request().Context(), middleware.Principal(c), id)
	})(c)
}
