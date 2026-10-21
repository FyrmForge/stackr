package admin

import (
	"strconv"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

func (h *handler) mountObserve(g *echo.Group, a *middleware.Access) {
	g.POST(ui.Base+"/observe", h.act("observability", func(c echo.Context) (string, *service.Job, error) {
		on := "false"
		if c.FormValue("observe_enabled") != "" {
			on = "true"
		}
		return "Saved.", nil, h.orch.SetSettings(c.Request().Context(), map[string]string{
			"observe_enabled":           on,
			"observe_metrics_retention": c.FormValue("observe_metrics_retention"),
			"observe_min_free":          c.FormValue("observe_min_free"),
		})
	}), a.Require("serverdefaults.set"))
}

// observeTab is metrics history's knobs and what it holds.
func (h *handler) observeTab(c echo.Context) (templ.Component, error) {
	s, err := h.orch.ObserveStatus(c.Request().Context())
	if err != nil {
		return nil, err
	}
	v := ui.ObserveView{
		Action:    ui.Base + "/observe",
		Enabled:   s.Enabled,
		Retention: strconv.Itoa(s.RetentionDays),
		MinFree:   strconv.FormatUint(s.MinFreeBytes>>30, 10),
		Err:       s.Error,
	}
	switch {
	case s.Container == "running":
		v.State = "running."
	case s.Container != "":
		v.State = s.Container + "."
	case s.Enabled && s.Error == "":
		v.State = "starting."
	default:
		v.State = "none."
	}
	if s.DataBytes > 0 {
		v.Data = render.Size(int64(s.DataBytes)) + "."
	}
	if s.DiskTotal > 0 {
		v.Free = render.Size(int64(s.DiskFree)) + " of " + render.Size(int64(s.DiskTotal)) + "."
	}
	return ui.Observe(v), nil
}
