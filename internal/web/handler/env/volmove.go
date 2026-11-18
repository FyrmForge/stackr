package env

import (
	"cmp"
	"net/http"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/drawer/volume"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// Move to disk in the volume drawer. The dialog is an answer into
// #vol-move-dlg: the plan POST (explicit, it measures the volume) draws the
// size and the room, a refused press draws them again with the reason, a press
// that queues the job answers the drawer.

// moveDialog is the plan as the dialog shows it. msg is the refusal of a press.
func (h *handler) moveDialog(c echo.Context, msg string) (volume.MoveDialog, error) {
	s := scope(c)
	base := render.EnvURL(c) + "/-/volumes/" + c.Param("volume")
	d := volume.MoveDialog{Where: s.Stack.Name + " / " + s.Env.Name, Move: base + "/move"}
	vol, err := h.orch.Volume(c.Request().Context(), c.Param("volume"))
	if err != nil {
		return d, err
	}
	d.Name = vol.Slug
	p, err := h.orch.PlanVolumeMove(c.Request().Context(), middleware.Principal(c), vol.ID)
	if err != nil {
		text, ok := render.Refused(err)
		if !ok {
			return d, err
		}
		d.Msg = cmp.Or(msg, text)
		return d, nil
	}
	d.Need, d.Free, d.Low = diskSize(p.Need), diskSize(int64(p.Free)), !p.Fits
	if p.SizeBytes < 0 && !p.Finish {
		d.Need = "unknown"
	}
	switch {
	case p.SizeBytes >= 0:
		d.Text = diskSize(p.SizeBytes) + " moves from Docker storage to the node disk."
	default:
		d.Text = "The volume moves from Docker storage to the node disk."
	}
	if t := stopped(p.Tiles); t != "" {
		d.Text += " " + t + " while it copies."
	}
	d.Msg = msg
	if d.Msg == "" && d.Low {
		d.Msg = "Not enough room. Needs " + d.Need + ", " + d.Free + " free."
	}
	return d, nil
}

// stopped is "web is stopped", "web and api are stopped"; "" for none.
func stopped(tiles []string) string {
	switch len(tiles) {
	case 0:
		return ""
	case 1:
		return tiles[0] + " is stopped"
	}
	return strings.Join(tiles[:len(tiles)-1], ", ") + " and " + tiles[len(tiles)-1] + " are stopped"
}

// moveRefusal words a refused press as the dialog does.
func moveRefusal(msg string) string {
	switch {
	case strings.Contains(msg, "job holds the data volume"):
		return "A backup is running. Try again when it is done."
	case msg == "The volume's mounts changed, try again.":
		return "The volume's mounts changed. Try again."
	}
	return msg
}

// POST …/-/volumes/:volume/move/plan: what the move needs.
func (h *handler) MoveToDiskPlan(c echo.Context) error {
	d, err := h.moveDialog(c, "")
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, http.StatusOK, volume.MoveDialogView(d))
}

// POST …/-/volumes/:volume/move: queue the move, or finish a cut-off one. A
// refusal of the dialog's press lands in the dialog; the strip's Finish move
// answers the drawer.
func (h *handler) MoveToDisk(c echo.Context) error {
	_, err := h.orch.MoveVolumeToDisk(c.Request().Context(), middleware.Principal(c), c.Param("volume"))
	if err == nil || c.FormValue("finish") != "" {
		return h.volume(c, "overview", "moving to the disk", err)
	}
	msg, ok := render.Refused(err)
	if !ok {
		// a backup in the way is a plain error, not a refusal: say it in the dialog
		if !strings.Contains(err.Error(), "job holds the data volume") {
			return middleware.HTTPError(err)
		}
		msg = err.Error()
	}
	d, derr := h.moveDialog(c, moveRefusal(msg))
	if derr != nil {
		return middleware.HTTPError(derr)
	}
	c.Response().Header().Set("HX-Retarget", "#vol-move-dlg")
	c.Response().Header().Set("HX-Reswap", "innerHTML")
	return respond.HTML(c, http.StatusOK, volume.MoveDialogView(d))
}

func diskSize(b int64) string { return render.DiskSize(b) }

// diskScreens fills the overview's node disk parts: the two-copies banner, the
// Move to disk button or the strip of a cut-off move, and the move's job. Rows
// and memory only: the drawer polls, and a poll never asks Docker.
func (h *handler) diskScreens(c echo.Context, vol service.Volume, o *volume.OverviewView) error {
	ctx, s := c.Request().Context(), scope(c)
	write := can(c, s, "tile.write")
	o.Ghost = h.ghost(c, vol.ID, vol.Slug, write)
	if !write {
		return nil
	}
	base := render.EnvURL(c) + "/-/volumes/" + vol.ID
	m, err := h.orch.VolumeMoveInfo(ctx, vol.ID)
	if err != nil {
		return err
	}
	if m.Offer {
		o.Move = &volume.MoveView{Plan: base + "/move/plan", Move: base + "/move", Why: m.Why, Disabled: m.Running || m.Why != "", Finish: m.Finish}
		if m.Finish {
			var names []string
			for _, mt := range o.Mounts {
				names = append(names, mt.Tile)
			}
			o.Move.Strip = "Move did not finish."
			if t := stopped(names); t != "" {
				o.Move.Strip += " " + t + "."
			}
		}
	}
	if j, ok, err := h.orch.VolumeMoveJob(ctx, vol.ID); err != nil {
		return err
	} else if ok {
		jv := render.JobView(render.EnvURL(c), j)
		jv.Live = false // the overview polls
		o.Job = &jv
	}
	return nil
}
