// Package tile serves the tile drawer: GET a tab (?tab=), POST an action
// (one verb, then the tab it belongs to again), and the drawer's log
// stream (a replica's or a run's).
package tile

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/tile"
	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler { return &handler{orch: orch} }

// Mount registers the drawer on g; every path is under /:org/:stack/:env.
func (h *handler) Mount(g *echo.Group, a *middleware.Access) {
	d := "/:org/:stack/:env/-/tiles/:tile"
	require := a.Require
	read, write := require("tile.read"), require("tile.write")
	g.GET(d, h.Drawer, read)
	g.GET(d+"/logs/stream", h.LogStream, read)
	for verb, f := range map[string]func(context.Context, string) (service.Job, error){
		"deploy": h.orch.Deploy, "restart": h.orch.RestartTile, "stop": h.orch.StopTile,
		"start": h.orch.StartTile, "delete": h.orch.DeleteTile,
	} {
		g.POST(d+"/"+verb, h.tileJob(verb, f), write)
	}
	g.POST(d+"/run", h.Run, write)
	g.POST(d+"/pause", h.Pause, write)
	g.POST(d+"/runs/:run/stop", h.StopRun, write)
	g.POST(d+"/domains", h.AttachDomain, require("domain.write"))
	g.POST(d+"/domains/:domain/detach", h.DetachDomain, require("domain.write"))
	g.POST(d+"/domains/:domain/raw", h.SetRawCaddy, require("proxy.admin"))
	g.POST(d+"/env", h.SetEnv, write)
	g.POST(d+"/settings", h.SetSettings, write)
	g.POST(d+"/image", h.SetImagePolicy, write)
	g.POST(d+"/image/check", h.CheckImage, write)
}

func tileOf(c echo.Context) *service.Tile { return middleware.ScopeOf(c).Tile }

func base(c echo.Context) string { return render.EnvURL(c) + "/-/tiles/" + tileOf(c).Slug }

func view(c echo.Context, tab string) ui.View {
	t := tileOf(c)
	if !slices.Contains(ui.Tabs(t.Kind), tab) {
		tab = "status"
	}
	return ui.View{Node: t.ID, Name: t.Name, Kind: t.Kind, Base: base(c), Tab: tab}
}

// GET …/-/tiles/:tile?tab=
func (h *handler) Drawer(c echo.Context) error { return h.show(c, http.StatusOK, view(c, c.QueryParam("tab"))) }

// show renders v's tab: each case is one read verb and its view.
func (h *handler) show(c echo.Context, status int, v ui.View) error {
	ctx, t := c.Request().Context(), tileOf(c)
	var body templ.Component
	switch v.Tab {
	case "status":
		s, err := h.orch.TileStatus(ctx, t.ID)
		if err != nil {
			return middleware.HTTPError(err)
		}
		body = ui.Status(v, statusView(render.EnvURL(c), s))
	case "logs":
		s, err := h.orch.TileStatus(ctx, t.ID)
		if err != nil {
			return middleware.HTTPError(err)
		}
		body = ui.Logs(v, logsView(c, v.Base, s))
	case "domains":
		ds, err := h.orch.Domains(ctx, t.ID)
		if err != nil {
			return middleware.HTTPError(err)
		}
		p := middleware.Principal(c)
		body = ui.Domains(v, domainsView(ds, p != nil && p.Access.Admin))
	case "env":
		body = ui.Env(v, ui.EnvView{JSON: t.EnvJSON})
	case "settings":
		body = ui.Settings(v, settingsView(v.Base, *t, nil))
	case "jobs":
		js, err := h.orch.TileJobs(ctx, []string{t.ID}, 20)
		if err != nil {
			return middleware.HTTPError(err)
		}
		body = ui.Jobs(v, jobsView(js))
	case "image":
		i, err := h.orch.TileImage(ctx, t.ID)
		if err != nil {
			return middleware.HTTPError(err)
		}
		body = ui.Image(v, imageView(*t, i))
	case "runs":
		rs, err := h.orch.Runs(ctx, t.ID, 20)
		if err != nil {
			return middleware.HTTPError(err)
		}
		body = ui.Runs(v, runsView(*t, rs))
	case "backups":
		vs, err := h.orch.TileVolumes(ctx, t.ID)
		if err != nil {
			return middleware.HTTPError(err)
		}
		body = ui.Backups(v, backupsView(render.EnvURL(c), vs))
	}
	return respond.HTML(c, status, ui.Drawer(v, body))
}

// after answers an action with its tab: a refusal shows over it (422), a
// done action says what it did.
func (h *handler) after(c echo.Context, tab, note string, err error) error {
	v := view(c, tab)
	if err != nil {
		msg, ok := render.Refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		v.Error = msg
		return h.show(c, http.StatusUnprocessableEntity, v)
	}
	v.Note = note
	return h.show(c, http.StatusOK, v)
}

func (h *handler) tileJob(verb string, f func(context.Context, string) (service.Job, error)) echo.HandlerFunc {
	return func(c echo.Context) error {
		_, err := f(c.Request().Context(), tileOf(c).ID)
		return h.after(c, "status", verb+" queued", err)
	}
}

func (h *handler) Run(c echo.Context) error {
	j, _, err := h.orch.RunTile(c.Request().Context(), tileOf(c).ID)
	note := "run queued"
	if err == nil && j.ID == "" {
		note = "refused: the last run is still going"
	}
	return h.after(c, "runs", note, err)
}

func (h *handler) Pause(c echo.Context) error {
	paused := c.FormValue("paused") == "true"
	_, err := h.orch.PauseTile(c.Request().Context(), tileOf(c).ID, paused)
	return h.after(c, "runs", map[bool]string{true: "schedule paused", false: "schedule resumed"}[paused], err)
}

func (h *handler) StopRun(c echo.Context) error {
	err := h.orch.StopRun(c.Request().Context(), tileOf(c).ID, c.Param("run"))
	return h.after(c, "runs", "run stopped", err)
}

func (h *handler) AttachDomain(c echo.Context) error {
	s := service.DomainSpec{Host: strings.TrimSpace(c.FormValue("host")), Path: c.FormValue("path")}
	if p := c.FormValue("port"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return h.after(c, "domains", "", errs.Invalidf("port", "must be a number"))
		}
		s.Port = n
	}
	_, err := h.orch.AttachDomain(c.Request().Context(), tileOf(c).ID, s)
	return h.after(c, "domains", "attached "+s.Host, err)
}

func (h *handler) DetachDomain(c echo.Context) error {
	err := h.orch.DetachDomain(c.Request().Context(), c.Param("domain"))
	return h.after(c, "domains", "detached", err)
}

func (h *handler) SetRawCaddy(c echo.Context) error {
	_, err := h.orch.SetRawCaddy(c.Request().Context(), c.Param("domain"), c.FormValue("raw_caddy"))
	return h.after(c, "domains", "snippet saved", err)
}

// update applies edit through UpdateTile and says whether it redeploys.
func (h *handler) update(c echo.Context, tab string, edit func(*service.Tile) error) error {
	t, j, err := h.orch.UpdateTile(c.Request().Context(), tileOf(c).ID, edit)
	note := "saved"
	if j != nil {
		note = "saved; redeploy queued"
	}
	if err == nil {
		*tileOf(c) = t // the tab shows the edited row, not the scope's
	}
	return h.after(c, tab, note, err)
}

func (h *handler) SetEnv(c echo.Context) error {
	env := c.FormValue("env_json")
	return h.update(c, "env", func(t *service.Tile) error { t.EnvJSON = env; return nil })
}

func (h *handler) SetImagePolicy(c echo.Context) error {
	up, tag := c.FormValue("update_policy"), c.FormValue("tag_policy")
	return h.update(c, "image", func(t *service.Tile) error { t.UpdatePolicy, t.TagPolicy = up, tag; return nil })
}

func (h *handler) CheckImage(c echo.Context) error {
	s := middleware.ScopeOf(c)
	_, err := h.orch.CheckImages(c.Request().Context(), s.Stack.ID, s.Tile.ID)
	return h.after(c, "image", "check queued", err)
}

// POST …/settings answers the settings form alone (it swaps itself).
func (h *handler) SetSettings(c echo.Context) error {
	cpu, mem := c.FormValue("cpu_limit"), c.FormValue("mem_limit_mb")
	t, _, err := h.orch.UpdateTile(c.Request().Context(), tileOf(c).ID, func(t *service.Tile) error {
		t.CPULimit, t.MemLimitMB = 0, 0
		if cpu != "" {
			f, err := strconv.ParseFloat(cpu, 64)
			if err != nil {
				return errs.Invalidf("cpu_limit", "must be a number")
			}
			t.CPULimit = f
		}
		if mem != "" {
			n, err := strconv.Atoi(mem)
			if err != nil {
				return errs.Invalidf("mem_limit_mb", "must be a whole number")
			}
			t.MemLimitMB = n
		}
		return nil
	})
	if err != nil {
		v, ok := errs.IsInvalid(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		t = *tileOf(c)
		t.CPULimit, t.MemLimitMB = 0, 0 // show what was typed, not the stored row
		sv := settingsView(base(c), t, map[string]string{v.Field: v.Msg})
		sv.Rows[0].Value, sv.Rows[1].Value = cpu, mem
		return respond.HTML(c, http.StatusUnprocessableEntity, comp.SettingsForm(sv))
	}
	return respond.HTML(c, http.StatusOK, comp.SettingsForm(settingsView(base(c), t, nil)))
}

// GET …/logs/stream?container=&run=: one rendered LogLine per "line".
func (h *handler) LogStream(e echo.Context) error {
	ctx := context.WithoutCancel(e.Request().Context())
	t := tileOf(e)
	var lines <-chan string
	var stop func()
	var err error
	if run := e.QueryParam("run"); run != "" {
		lines, stop, err = h.orch.FollowRunLog(ctx, t.ID, run, 200)
	} else {
		lines, stop, err = h.orch.FollowLogs(ctx, t.ID, e.QueryParam("container"), 200)
	}
	if err != nil {
		return middleware.HTTPError(err)
	}
	out, done := make(chan stream.HTML), make(chan struct{})
	go func() {
		defer close(out)
		for l := range lines {
			b, err := render.Event(ctx, comp.LogLine(logLine(l)))
			if err != nil {
				return
			}
			select {
			case out <- b:
			case <-done:
				return
			}
		}
	}()
	return stream.Lines(e, out, func() { close(done); stop() })
}

// logLine splits docker's RFC 3339 timestamp off a line when there is
// one. ponytail: the level is not parsed; the pane's level filter sees "".
func logLine(l string) comp.LogLineView {
	if ts, rest, ok := strings.Cut(l, " "); ok {
		if at, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			return comp.LogLineView{Time: at.Format("15:04:05"), Text: rest}
		}
	}
	return comp.LogLineView{Text: l}
}
