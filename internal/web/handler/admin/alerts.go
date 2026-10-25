package admin

import (
	"strings"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
)

func (h *handler) mountAlerts(g *echo.Group, a *middleware.Access) {
	b, set := ui.Base, a.Require("serverdefaults.set")
	// POST /-/admin/alerts: a blank URL or secret keeps the stored one.
	g.POST(b+"/alerts", h.act("alerts", func(c echo.Context) (string, *service.Job, error) {
		var secret *string
		if s := c.FormValue("secret"); s != "" {
			secret = &s
		}
		return "Saved.", nil, h.orch.SetAlerts(c.Request().Context(), c.FormValue("url"), secret)
	}), set)
	// POST /-/admin/alerts/test
	g.POST(b+"/alerts/test", h.act("alerts", func(c echo.Context) (string, *service.Job, error) {
		return "Test alert delivered.", nil, h.orch.TestAlert(c.Request().Context())
	}), set)
	// POST /-/admin/alerts/clear
	g.POST(b+"/alerts/clear", h.act("alerts", func(c echo.Context) (string, *service.Job, error) {
		return "Alerts are off.", nil, h.orch.ClearAlerts(c.Request().Context())
	}), set)
	// POST /-/admin/alerts/heartbeat: a blank URL keeps the stored one.
	g.POST(b+"/alerts/heartbeat", h.act("alerts", func(c echo.Context) (string, *service.Job, error) {
		u := strings.TrimSpace(c.FormValue("url"))
		if u == "" {
			return "Saved.", nil, nil
		}
		return "Saved.", nil, h.orch.SetHeartbeat(c.Request().Context(), u)
	}), set)
	// POST /-/admin/alerts/heartbeat/clear
	g.POST(b+"/alerts/heartbeat/clear", h.act("alerts", func(c echo.Context) (string, *service.Job, error) {
		return "Heartbeat is off.", nil, h.orch.SetHeartbeat(c.Request().Context(), "")
	}), set)
}

func (h *handler) alertsTab(c echo.Context) (templ.Component, error) {
	s, err := h.orch.AlertSettings(c.Request().Context())
	v := ui.AlertsView{Action: ui.Base + "/alerts", URL: s.URL, SecretSet: s.SecretSet}
	v.Heartbeat, v.HeartbeatSave = s.Heartbeat, ui.Base+"/alerts/heartbeat"
	if s.Heartbeat != "" {
		v.HeartbeatClear = ui.Base + "/alerts/heartbeat/clear"
	}
	if s.URL != "" {
		v.Test = ui.Base + "/alerts/test"
		v.Clear = comp.ConfirmView{
			Button:  "Turn off",
			Title:   "Turn alerts off?",
			Warning: "stackr forgets the webhook URL and secret.",
			Action:  ui.Base + "/alerts/clear",
			Target:  "#" + comp.DrawerRoot,
		}
	}
	if d := s.Last; d != nil {
		v.LastOK = d.OK
		v.Last, v.LastAt = "ok", d.At
		if !d.OK {
			v.Last = "failed, " + d.Reason
		}
	}
	return ui.Alerts(v), err
}
