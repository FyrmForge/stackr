package env

import (
	"net/http"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/drawer/volume"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// Wipe, delete and restore of a volume in its drawer. The service decides;
// these bind the ticks, call one verb and draw its answer.

// managedHint is the one line a stack file's removal gets.
const managedHint = "Add it back to the stack file to keep it."

// POST …/-/volumes/:volume/wipe takes the ticks stop and backup.
func (h *handler) WipeVolume(c echo.Context) error {
	_, err := h.orch.WipeVolume(c.Request().Context(), c.Param("volume"), c.FormValue("stop") == "on", c.FormValue("backup") == "on")
	if err != nil {
		return h.volume(c, "settings", "", err)
	}
	return h.volume(c, "overview", "", nil) // the job line
}

// POST …/-/volumes/:volume/delete takes the tick backup. Done at once, the
// drawer closes with the canvas; a job keeps the drawer on its job line.
func (h *handler) DeleteVolume(c echo.Context) error {
	j, err := h.orch.DeleteVolume(c.Request().Context(), c.Param("volume"), c.FormValue("backup") == "on")
	switch {
	case err != nil:
		return h.volume(c, "settings", "", err)
	case j.ID == "":
		hamrmw.SetFlash(c, "Volume deleted.", hamrmw.FlashInfo)
		return volumeGone(c)
	}
	return h.volume(c, "overview", "", nil)
}

// POST …/-/volumes/:volume/restore-volume takes a removed volume back.
func (h *handler) RestoreVolume(c echo.Context) error {
	if err := h.orch.RestoreVolume(c.Request().Context(), c.Param("volume")); err != nil {
		return h.volume(c, "overview", "", err)
	}
	return h.volume(c, "overview", "Volume restored.", nil)
}

// volumeGone sends the drawer of a deleted volume back to its env's canvas,
// where the card is already gone.
func volumeGone(c echo.Context) error {
	c.Response().Header().Set("HX-Redirect", render.EnvURL(c))
	return c.NoContent(http.StatusOK)
}

// dataJob is the volume's newest wipe or delete job as the overview's status
// line, unless a move's job already holds the slot. The overview polls.
func (h *handler) dataJob(c echo.Context, id string, o *volume.OverviewView) error {
	if o.Job != nil {
		return nil
	}
	j, ok, err := h.orch.VolumeDataJob(c.Request().Context(), id)
	if err != nil || !ok {
		return err
	}
	jv := render.JobViewNote(c.Request().Context(), h.orch, render.EnvURL(c), j)
	jv.Live = false
	o.Job = &jv
	return nil
}

// removal is what the drawer says of a volume a stack file dropped; nil for
// a volume in use and for a move's old copy.
func (h *handler) removal(c echo.Context, vol service.Volume, base string) (*volume.Removal, error) {
	goes, err := h.orch.OrphanGoesOn(c.Request().Context(), vol)
	if err != nil || goes.IsZero() {
		return nil, err
	}
	r := &volume.Removal{On: vol.OrphanedAt.Format("02-01"), Goes: goes.Format("02-01"), Restore: base + "/restore-volume"}
	if scope(c).Stack.ConfigRepo != "" {
		r.Hint = managedHint
	}
	return r, nil
}

// volumeData is what the Settings tab offers on the volume's data.
func (h *handler) volumeData(c echo.Context, vol service.Volume, base string) (volume.Data, error) {
	switch {
	case vol.OrphanedAt != nil:
		r, err := h.removal(c, vol, base)
		if err != nil || r == nil {
			return volume.Data{}, err // a move's old copy: nothing to offer
		}
		return volume.Data{Restore: r.Restore, Hint: r.Hint}, nil
	}
	d := volume.Data{Wipe: vol.ScopeKind == "env" && vol.InstanceID == nil, Delete: true}
	if dest, err := h.orch.BackupFirstDest(c.Request().Context(), vol.ID); err == nil {
		d.Dest = render.DestName(dest)
	} // else no Back up first tick: the dialog still works without it
	return d, nil
}
