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
		logs := "false"
		if c.FormValue("observe_logs_enabled") != "" {
			logs = "true"
		}
		flag := func(k string) string {
			if c.FormValue(k) != "" {
				return "true"
			}
			return "false"
		}
		return "Saved.", nil, h.orch.SetSettings(c.Request().Context(), map[string]string{
			"observe_enabled":           on,
			"observe_metrics_retention": c.FormValue("observe_metrics_retention"),
			"observe_min_free":          c.FormValue("observe_min_free"),
			"observe_logs_enabled":      logs,
			"observe_logs_retention":    c.FormValue("observe_logs_retention"),
			"observe_logs_max_size":     c.FormValue("observe_logs_max_size"),
			"observe_access_query":      flag("observe_access_query"),
			"observe_access_ip":         flag("observe_access_ip"),
			"observe_access_headers":    flag("observe_access_headers"),
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

		LogsEnabled: s.LogsEnabled,
		LogsDays:    strconv.Itoa(s.LogsDays),
		LogsMax:     strconv.FormatUint(s.LogsMaxBytes>>30, 10),

		AccessQuery: s.AccessQuery, AccessIP: s.AccessIP, AccessHeaders: s.AccessHeaders,
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
	if s.LogsBytes > 0 {
		v.LogsData = render.Size(int64(s.LogsBytes)) + " of " + render.Size(int64(s.LogsMaxBytes)) + "."
	}
	if s.DiskTotal > 0 {
		v.Free = render.Size(int64(s.DiskFree)) + " of " + render.Size(int64(s.DiskTotal)) + "."
	}
	return ui.Observe(v), nil
}
