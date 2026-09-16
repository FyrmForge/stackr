package settings

import (
	"errors"
	"net/http"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
)

// Panel upgrades (docs/plans/43-panel-upgrade.md). The work is in
// service.AdminService; these only render it.

// GET /admin/update
func (h *handler) Update(c echo.Context) error {
	ctx := c.Request().Context()
	v := upgradeView{Version: h.admin.Version(), Upgradable: h.admin.Upgradable()}
	if v.Upgradable {
		if _, err := h.admin.CheckUpgrade(ctx); err != nil {
			v.CheckErr = err.Error()
		}
		v.Latest, v.Available = h.admin.Available(ctx)
	}
	v.Archive, _ = h.store.GetSetting(ctx, service.SettingUpgradeArchive)
	v.Previous, _ = h.store.GetSetting(ctx, service.SettingUpgradePrevious)
	return respond.HTML(c, http.StatusOK, upgradePage(c, v))
}

// POST /admin/update
//
// Answers with a fragment that waits for the new build. By the time it lands
// swarm is already replacing this task.
func (h *handler) RunUpdate(c echo.Context) error {
	tag := c.FormValue("version")
	if err := h.admin.Upgrade(c.Request().Context(), tag); err != nil {
		msg := "Upgrade failed: " + err.Error()
		if errors.Is(err, service.ErrUpgradeRunning) {
			msg = "An upgrade is already running."
		}
		middleware.SetFlash(c, msg, middleware.FlashError)
		return respond.Redirect(c, "/admin/update")
	}
	return respond.HTML(c, http.StatusOK, upgradeWait(tag, h.admin.Version()))
}

// GET /admin/update/badge, the rail's update arrow. Reads the cached check
// only: it loads on every page.
func (h *handler) UpdateBadge(c echo.Context) error {
	tag, ok := h.admin.Available(c.Request().Context())
	if !ok {
		tag = ""
	}
	return respond.HTML(c, http.StatusOK, upgradeBadge(tag))
}
