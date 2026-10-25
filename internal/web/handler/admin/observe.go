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
	g.POST(ui.Base+"/observe", h.act("metrics", func(c echo.Context) (string, *service.Job, error) {
		flag := func(k string) string {
			if c.FormValue(k) != "" {
				return "true"
			}
			return "false"
		}
		all := map[string]string{
			"observe_enabled":           flag("observe_enabled"),
			"observe_metrics_retention": c.FormValue("observe_metrics_retention"),
			"observe_min_free":          c.FormValue("observe_min_free"),
			"observe_max_memory":        c.FormValue("observe_max_memory"),
			"observe_logs_enabled":      flag("observe_logs_enabled"),
			"observe_logs_retention":    c.FormValue("observe_logs_retention"),
			"observe_logs_max_size":     c.FormValue("observe_logs_max_size"),
			"observe_access_query":      flag("observe_access_query"),
			"observe_access_ip":         flag("observe_access_ip"),
			"observe_access_headers":    flag("observe_access_headers"),
		}
		// each panel posts its part: a box left out of the post is not "off"
		vals := all
		if keys, ok := observeParts[c.FormValue("part")]; ok {
			vals = map[string]string{}
			for _, k := range keys {
				vals[k] = all[k]
			}
		}
		return "Saved.", nil, h.orch.SetSettings(c.Request().Context(), vals)
	}), a.Require("serverdefaults.set"))
}

// observeParts is each Metrics and logs panel's knobs.
var observeParts = map[string][]string{
	"metrics": {"observe_enabled", "observe_metrics_retention", "observe_min_free", "observe_max_memory"},
	"logs":    {"observe_logs_enabled", "observe_logs_retention", "observe_logs_max_size"},
	"access":  {"observe_access_query", "observe_access_ip", "observe_access_headers"},
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
		Memory:    strconv.FormatUint(s.MemoryBytes>>20, 10),
		Err:       s.Error,

		LogsEnabled: s.LogsSet, // not s.LogsEnabled: with metrics off a save would turn logs off
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
	if s.LogsDropped > 0 {
		v.LogsDropped = strconv.FormatUint(s.LogsDropped, 10) + " since the panel started."
	}
	if s.DiskTotal > 0 {
		v.Free = render.Size(int64(s.DiskFree)) + " of " + render.Size(int64(s.DiskTotal)) + "."
	}
	return ui.Observe(v), nil
}
