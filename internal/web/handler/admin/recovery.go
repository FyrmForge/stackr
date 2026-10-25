package admin

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// The recovery key section of Admin > Backups. Each action answers with the
// section alone; showing or rotating the key needs the admin's password.

// recoveryView is the section as the Backups tab first renders it.
func (h *handler) recoveryView() ui.RecoveryView {
	gen, unsaved := h.orch.RecoveryStatus()
	v := ui.RecoveryView{
		Gen:     gen,
		Unsaved: unsaved,
		Show:    ui.Base + "/backups/recovery/show",
		Saved:   ui.Base + "/backups/recovery/saved",
	}
	if !unsaved {
		v.Rotate = ui.Base + "/backups/recovery/rotate"
	}
	return v
}

// ShowRecoveryKey is POST /-/admin/backups/recovery/show.
func (h *handler) ShowRecoveryKey(c echo.Context) error {
	return h.recoveryAnswer(c, func() (string, error) {
		return h.orch.RevealRecoveryKey(c.Request().Context(), middleware.Principal(c).User.ID, c.FormValue("password"))
	})
}

// RotateRecoveryKey is POST /-/admin/backups/recovery/rotate.
func (h *handler) RotateRecoveryKey(c echo.Context) error {
	return h.recoveryAnswer(c, func() (string, error) {
		return h.orch.RotateRecoveryKey(c.Request().Context(), middleware.Principal(c).User.ID, c.FormValue("password"))
	})
}

// SavedRecoveryKey is POST /-/admin/backups/recovery/saved.
func (h *handler) SavedRecoveryKey(c echo.Context) error {
	if err := h.orch.ConfirmRecoveryKey(); err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, http.StatusOK, ui.Recovery(h.recoveryView()))
}

// recoveryAnswer runs an action that yields the private half: the section
// comes back with the key on success and the refusal on a wrong password.
func (h *handler) recoveryAnswer(c echo.Context, do func() (string, error)) error {
	v, status := h.recoveryView(), http.StatusOK
	key, err := do()
	if err != nil {
		msg, ok := render.Refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		v.Error, status = msg, http.StatusUnprocessableEntity
	} else {
		v.Key = key
		v.Gen, v.Unsaved = h.orch.RecoveryStatus()
	}
	return respond.HTML(c, status, ui.Recovery(v))
}
