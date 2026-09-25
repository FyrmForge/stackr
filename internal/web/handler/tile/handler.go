// Package tile serves the tile drawer: GET a tab (?tab=), POST an action
// (one verb, then the tab it belongs to again), and the drawer's log
// stream (a replica's or a run's).
package tile

import (
	"context"
	"maps"
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
		"deploy":  h.orch.Deploy,
		"restart": h.orch.RestartTile,
		"stop":    h.orch.StopTile,
		"start":   h.orch.StartTile,
		"delete":  h.orch.DeleteTile,
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
	g.POST(d+"/image/check", h.CheckImage, write)
}

func tileOf(c echo.Context) *service.Tile {
	return middleware.ScopeOf(c).Tile
}

func base(c echo.Context) string {
	return render.EnvURL(c) + "/-/tiles/" + tileOf(c).Slug
}

// head is what every answer's header reads, once per request.
type head struct {
	status service.TileStatus
	image  service.Image // a pulled tile's watch; zero otherwise
}

// typed is a refused settings save: what was posted and why, drawn over
// the stored row.
type typed struct {
	vals, errors map[string]string
}

// view is the drawer frame for tab: v0's header (status, source, where it
// runs, the image strip) over the tab the kind has.
func (h *handler) view(c echo.Context, tab string) (ui.View, head, error) {
	ctx, s := c.Request().Context(), middleware.ScopeOf(c)
	t := s.Tile
	st, err := h.orch.TileStatus(ctx, t.ID)
	if err != nil {
		return ui.View{}, head{}, err
	}
	v := ui.View{
		Node:     t.ID,
		Name:     t.Name,
		Kind:     t.Kind,
		Source:   source(*t),
		Base:     base(c),
		Tab:      ui.Tab(t.Kind, tab),
		Status:   st.Word,
		Stopped:  st.Word == "stopped",
		Paused:   t.Paused,
		Location: s.Stack.Name + " / " + s.Env.Name,
		EnvColor: s.Env.Color,
	}
	hd := head{status: st}
	if !pulls(*t) {
		return v, hd, nil
	}
	if hd.image, err = h.orch.TileImage(ctx, t.ID); err != nil {
		return v, hd, err
	}
	if hd.image.Newer() {
		v.NewDigest = hd.image.LastDigest
		v.AutoUpdate = t.UpdatePolicy == "auto"
	}
	return v, hd, nil
}

// GET …/-/tiles/:tile?tab=
func (h *handler) Drawer(c echo.Context) error {
	v, hd, err := h.view(c, c.QueryParam("tab"))
	if err != nil {
		return middleware.HTTPError(err)
	}
	return h.show(c, http.StatusOK, v, hd, nil)
}

// show renders v's tab: each case is one read verb and its view.
func (h *handler) show(c echo.Context, status int, v ui.View, hd head, ty *typed) error {
	ctx, t := c.Request().Context(), tileOf(c)
	var body templ.Component
	switch v.Tab {
	case "status":
		ds, err := h.orch.Domains(ctx, t.ID)
		if err != nil {
			return middleware.HTTPError(err)
		}
		sv := statusView(render.EnvURL(c), *t, hd.status, ds)
		if sv.Job != nil {
			sv.Job.Refresh = v.Base + "?tab=status"
		}
		body = ui.Status(v, sv)
	case "logs":
		last := ""
		if runKind(t.Kind) {
			rs, err := h.orch.Runs(ctx, t.ID, 1)
			if err != nil {
				return middleware.HTTPError(err)
			}
			if len(rs) > 0 {
				last = rs[0].ID
			}
		}
		body = ui.Logs(v, logsView(c, v.Base, *t, hd.status, last))
	case "env":
		body = ui.Env(v, envView(t.EnvJSON))
	case "settings":
		sv, err := h.settings(c, *t, hd)
		if err != nil {
			return middleware.HTTPError(err)
		}
		if ty != nil {
			maps.Copy(sv.Vals, ty.vals)
			sv.Errors = ty.errors
		}
		body = ui.Settings(v, sv)
	case "jobs":
		js, err := h.orch.TileJobs(ctx, []string{t.ID}, 20)
		if err != nil {
			return middleware.HTTPError(err)
		}
		body = ui.Jobs(v, jobsView(js))
	case "runs":
		rs, err := h.orch.Runs(ctx, t.ID, 20)
		if err != nil {
			return middleware.HTTPError(err)
		}
		body = ui.Runs(v, runsView(v.Base, hd.status, rs))
	case "backups":
		vs, err := h.orch.TileVolumes(ctx, t.ID)
		if err != nil {
			return middleware.HTTPError(err)
		}
		body = ui.Backups(v, backupsView(render.EnvURL(c), vs))
	}
	return respond.HTML(c, status, ui.Drawer(v, body))
}

// settings is the Settings tab: the form over the row, the domains of a
// kind with an endpoint, a pulled image's watch.
func (h *handler) settings(c echo.Context, t service.Tile, hd head) (ui.SettingsView, error) {
	sv := settingsView(t)
	if !runKind(t.Kind) {
		ds, err := h.orch.Domains(c.Request().Context(), t.ID)
		if err != nil {
			return sv, err
		}
		p := middleware.Principal(c)
		sv.Domains = &ui.DomainsView{Rows: domainRows(ds), Admin: p != nil && p.Access.Admin}
	}
	if pulls(t) {
		iv := imageView(t, hd.image)
		sv.Image = &iv
	}
	return sv, nil
}

// after answers an action with its tab: a refusal shows over it (422), a
// done action says what it did.
func (h *handler) after(c echo.Context, tab, note string, err error) error {
	v, hd, verr := h.view(c, tab)
	if verr != nil {
		return middleware.HTTPError(verr)
	}
	if err != nil {
		msg, ok := render.Refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		v.Error = msg
		return h.show(c, http.StatusUnprocessableEntity, v, hd, nil)
	}
	v.Note = note
	return h.show(c, http.StatusOK, v, hd, nil)
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
	t, err := h.orch.PauseTile(c.Request().Context(), tileOf(c).ID, paused)
	if err == nil {
		*tileOf(c) = t // the header offers the other one
	}
	return h.after(c, "runs", map[bool]string{true: "schedule paused", false: "schedule resumed"}[paused], err)
}

func (h *handler) StopRun(c echo.Context) error {
	err := h.orch.StopRun(c.Request().Context(), tileOf(c).ID, c.Param("run"))
	return h.after(c, "runs", "run stopped", err)
}

// AttachDomain takes the literal form, or the auto one: the orchestrator
// names an auto host.
func (h *handler) AttachDomain(c echo.Context) error {
	if c.FormValue("auto") != "" {
		d, err := h.orch.AttachDomain(c.Request().Context(), tileOf(c).ID, service.DomainSpec{Auto: true})
		return h.after(c, "settings", "attached "+d.Host, err)
	}
	https := c.FormValue("https") != ""
	s := service.DomainSpec{
		Host:       strings.TrimSpace(c.FormValue("host")),
		Path:       c.FormValue("path"),
		RedirectTo: strings.TrimSpace(c.FormValue("redirect_to")),
		HTTPS:      &https,
	}
	if p := c.FormValue("port"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return h.after(c, "settings", "", errs.Invalidf("port", "must be a number"))
		}
		s.Port = n
	}
	_, err := h.orch.AttachDomain(c.Request().Context(), tileOf(c).ID, s)
	return h.after(c, "settings", "attached "+s.Host, err)
}

func (h *handler) DetachDomain(c echo.Context) error {
	err := h.orch.DetachDomain(c.Request().Context(), c.Param("domain"))
	return h.after(c, "settings", "detached", err)
}

func (h *handler) SetRawCaddy(c echo.Context) error {
	_, err := h.orch.SetRawCaddy(c.Request().Context(), c.Param("domain"), c.FormValue("raw_caddy"))
	return h.after(c, "settings", "snippet saved", err)
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

// POST …/env: the editor's KEY=VALUE lines, or one row's × (drop).
func (h *handler) SetEnv(c echo.Context) error {
	drop, text := c.FormValue("drop"), c.FormValue("env")
	return h.update(c, "env", func(t *service.Tile) error {
		if drop != "" {
			t.EnvJSON = without(t.EnvJSON, drop)
			return nil
		}
		blob, err := withLines("env", t.EnvJSON, text)
		if err != nil {
			return err
		}
		t.EnvJSON = blob
		return nil
	})
}

func (h *handler) CheckImage(c echo.Context) error {
	s := middleware.ScopeOf(c)
	_, err := h.orch.CheckImages(c.Request().Context(), s.Stack.ID, s.Tile.ID)
	return h.after(c, "settings", "check queued", err)
}

// POST …/settings: the one settings form. A refusal draws the form again
// with what was typed, the reason under its field when the form has it.
func (h *handler) SetSettings(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	keys := posted(form)
	t, j, err := h.orch.UpdateTile(c.Request().Context(), tileOf(c).ID, func(t *service.Tile) error {
		return apply(t, keys, form)
	})
	if err == nil {
		*tileOf(c) = t
		note := "saved"
		if j != nil {
			note = "saved; redeploy queued"
		}
		return h.after(c, "settings", note, nil)
	}
	msg, ok := render.Refused(err)
	if !ok {
		return middleware.HTTPError(err)
	}
	v, hd, verr := h.view(c, "settings")
	if verr != nil {
		return middleware.HTTPError(verr)
	}
	shown := carries[tileOf(c).Kind]
	ty := &typed{vals: map[string]string{}, errors: map[string]string{}}
	for _, k := range keys {
		if slices.Contains(shown, k) {
			ty.vals[k] = strings.TrimSpace(form.Get(k))
		}
	}
	bad, invalid := errs.IsInvalid(err)
	if k := formKey(bad.Field); invalid && slices.Contains(shown, k) {
		ty.errors[k] = bad.Msg
	} else {
		v.Error = msg
	}
	return h.show(c, http.StatusUnprocessableEntity, v, hd, ty)
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
	return stream.Lines(e, out, func() {
		close(done)
		stop()
	})
}

// logLine splits docker's RFC 3339 timestamp off a line when there is
// one; NewLogLine reads the level.
func logLine(l string) comp.LogLineView {
	rest := l
	for _, stream := range []string{"O ", "E "} { // docker's stdout / stderr mark
		if s, ok := strings.CutPrefix(l, stream); ok {
			rest = s
		}
	}
	if ts, msg, ok := strings.Cut(rest, " "); ok {
		if at, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			return comp.NewLogLine(at.Format("15:04:05"), msg)
		}
	}
	return comp.NewLogLine("", l)
}
