// Package env is the env canvas's routes that are not the canvas itself:
// the env-level drawers and dialogs (slice, volume, proxy, create tile,
// rollback) and the drawers' job stream. Every handler is one verb call and a
// view struct.
package env

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/dialog"
	"github.com/FyrmForge/stackr/internal/ui/drawer/instance"
	"github.com/FyrmForge/stackr/internal/ui/drawer/proxy"
	"github.com/FyrmForge/stackr/internal/ui/drawer/slice"
	"github.com/FyrmForge/stackr/internal/ui/drawer/volume"
	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler { return &handler{orch: orch} }

func scope(c echo.Context) service.Scope { return middleware.ScopeOf(c) }

// Mount registers the env's drawers and dialogs on g, under the env
// page's "/-/" (never a slug). The env stream is the canvas's.
func (h *handler) Mount(g *echo.Group, a *middleware.Access) {
	e := "/:org/:stack/:env/-"
	g.GET(e+"/jobs/:job/events", h.JobEvents, a.Require("deployment.read"))
	g.POST(e+"/rollback/:release", h.Rollback, a.Require("env.write"))
	g.GET(e+"/new-tile", h.NewTile, a.Require("tile.write"))
	g.POST(e+"/new-tile", h.CreateTile, a.Require("tile.write"))
	g.GET(e+"/instances/:tile", h.Instance, a.Require("tile.read"))
	g.GET(e+"/slices/:provision", h.Slice, a.Require("tile.read"))
	g.POST(e+"/slices/:provision/detach", h.DetachSlice, a.Require("tile.write"))
	g.GET(e+"/volumes/:volume", h.Volume, a.Require("org.read"))
	g.POST(e+"/volumes/:volume/backup", h.BackupNow, a.Require("backup.write"))
	g.POST(e+"/volumes/:volume/restore", h.Restore, a.Require("backup.write"))
	g.POST(e+"/volumes/:volume/delete", h.DeleteVolume, a.Require("tile.write"))
	g.GET(e+"/proxy", h.Proxy, a.Require("tile.read"))
}

func when(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Local().Format("Jan 2 15:04")
}

// refused is an action's error as the drawer shows it; a real failure
// goes to the error page instead (ok false).
func refused(err error) (msg string, status int, fail error) {
	if err == nil {
		return "", http.StatusOK, nil
	}
	msg, ok := render.Refused(err)
	if !ok {
		return "", 0, middleware.HTTPError(err)
	}
	return msg, http.StatusUnprocessableEntity, nil
}

// GET …/-/jobs/:job/events: the job's status body as "update"
// whenever it changes, the final one included, then "end".
func (h *handler) JobEvents(c echo.Context) error {
	id, env := c.Param("job"), render.EnvURL(c)
	last, seq, ended := stream.HTML(""), int64(0), false
	return stream.PollAs(c, "update", func(ctx context.Context, _ int64) (any, int64, bool, error) {
		if ended {
			return last, seq, true, nil
		}
		j, err := h.orch.GetJob(ctx, id)
		if err != nil {
			return nil, 0, false, err
		}
		b, err := render.Event(ctx, comp.JobStatusBody(render.JobView(env, j)))
		if err != nil {
			return nil, 0, false, err
		}
		if b != last {
			last, seq = b, seq+1
		}
		ended = j.FinishedAt != nil
		return last, seq, false, nil
	})
}

// POST …/-/rollback/:release answers with the job's live status for
// the caller's target.
func (h *handler) Rollback(c echo.Context) error {
	j, err := h.orch.Rollback(c.Request().Context(), scope(c).Env.ID, c.Param("release"))
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, http.StatusOK, comp.JobStatus(render.JobView(render.EnvURL(c), j)))
}

// ---- create tile ----

func createView(c echo.Context) dialog.CreateTileView {
	url := render.EnvURL(c) + "/-/new-tile"
	v := dialog.CreateTileView{Action: url, Switch: url, Source: c.FormValue("source"), Name: c.FormValue("name"),
		Image: c.FormValue("image_ref"), GitURL: c.FormValue("git_url"), Branch: c.FormValue("git_branch"),
		Schedule: c.FormValue("schedule"), Trigger: c.FormValue("trigger"), Engine: c.FormValue("engine")}
	if !slices.ContainsFunc(dialog.Sources, func(o dialog.Option) bool { return o.Value == v.Source }) {
		v.Source = "image"
	}
	return v
}

// GET …/-/new-tile: the form, or it again for another source (the
// switch re-sends what was typed).
func (h *handler) NewTile(c echo.Context) error {
	cs, err := h.orch.Connectors(c.Request().Context(), scope(c).Org.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	v := createView(c)
	var hosts []string
	for _, x := range cs {
		hosts = append(hosts, x.Host)
	}
	v.Hosts = strings.Join(hosts, ", ")
	return respond.HTML(c, http.StatusOK, dialog.CreateTile(v))
}

// POST …/-/new-tile creates the tile and opens its drawer on the env
// page; a refusal re-renders the form with the field marked (422).
func (h *handler) CreateTile(c echo.Context) error {
	v, s := createView(c), scope(c)
	t := service.Tile{StackID: s.Stack.ID, EnvironmentID: s.Env.ID, Name: v.Name, Kind: v.Source}
	var err error
	switch v.Source {
	case "managed":
		t, err = h.orch.CreateManagedTile(c.Request().Context(), t, v.Engine)
	default:
		t.ImageRef, t.GitURL, t.GitBranch, t.Schedule, t.Trigger = v.Image, v.GitURL, v.Branch, v.Schedule, v.Trigger
		if v.Source == "service" {
			t.ImageRef = ""
		} else {
			t.GitURL, t.GitBranch = "", ""
		}
		t, err = h.orch.CreateTile(c.Request().Context(), t)
	}
	if err != nil {
		msg, ok := render.Refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		field := "general"
		if inv, ok := errs.IsInvalid(err); ok && inv.Field != "" {
			field, msg = inv.Field, inv.Msg
		}
		v.Errors = map[string]string{field: msg}
		if field != "general" && !slices.Contains([]string{"name", "image_ref", "git_url", "git_branch", "schedule", "engine"}, field) {
			v.Errors["general"] = field + ": " + msg // a field the form does not show
		}
		return respond.HTML(c, http.StatusUnprocessableEntity, dialog.CreateTile(v))
	}
	c.Response().Header().Set("HX-Redirect", render.EnvURL(c)+"?drawer="+t.ID+"&tab=status")
	return c.NoContent(http.StatusOK)
}

// ---- managed instance, slice, proxy ----

// GET …/-/instance/:tile
func (h *handler) Instance(c echo.Context) error {
	t := scope(c).Tile
	m, ps, err := h.orch.InstanceSlices(c.Request().Context(), t.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	v := instance.View{Name: t.Name, Engine: m.Engine, Scope: m.ScopeKind, Endpoint: m.Endpoint, AdminUser: m.AdminUser}
	for _, p := range ps {
		v.Slices = append(v.Slices, instance.SliceRow{ID: p.ID, Name: p.Slug, DB: p.DBName, OnRemove: p.OnRemove,
			Public: p.Public, Orphan: p.ConsumerTileID == nil, Drawer: render.EnvURL(c) + "/-/slices/" + p.ID + "?tab=bindings"})
	}
	return respond.HTML(c, http.StatusOK, instance.Slices(v))
}

// GET …/-/slice/:provision
func (h *handler) Slice(c echo.Context) error { return h.slice(c, nil) }

func (h *handler) DetachSlice(c echo.Context) error {
	_, err := h.orch.DetachSlice(c.Request().Context(), c.Param("provision"))
	return h.slice(c, err)
}

func (h *handler) slice(c echo.Context, actErr error) error {
	msg, status, fail := refused(actErr)
	if fail != nil {
		return fail
	}
	p, err := h.orch.Provision(c.Request().Context(), c.Param("provision"))
	if err != nil {
		return middleware.HTTPError(err)
	}
	var outs map[string]json.RawMessage
	_ = json.Unmarshal([]byte(p.Outputs), &outs) // names only; a bad blob shows none
	v := slice.View{Name: p.Slug, DB: p.DBName, User: p.DBUser, OnRemove: p.OnRemove, Public: p.Public,
		Consumer: p.ConsumerTileID != nil, Error: msg}
	for k := range outs {
		v.Outputs = append(v.Outputs, k)
	}
	slices.Sort(v.Outputs)
	if v.Consumer {
		v.Detach = render.EnvURL(c) + "/-/slices/" + p.ID + "/detach"
	}
	return respond.HTML(c, status, slice.Bindings(v))
}

// GET …/-/proxy
func (h *handler) Proxy(c echo.Context) error {
	rs, err := h.orch.Routes(c.Request().Context(), scope(c).Env.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	var v proxy.View
	for _, r := range rs {
		v.Rows = append(v.Rows, proxy.Route{Host: r.Host, Path: r.Path, Tile: r.Tile, Port: fmt.Sprint(r.ContainerPort),
			HTTPS: r.HTTPS, Auto: r.Auto, Raw: r.RawCaddy != ""})
	}
	return respond.HTML(c, http.StatusOK, proxy.Routes(v))
}

// ---- volume ----

// GET …/-/volume/:volume
func (h *handler) Volume(c echo.Context) error { return h.volume(c, "", nil) }

func (h *handler) BackupNow(c echo.Context) error {
	_, err := h.orch.BackupNow(c.Request().Context(), c.Param("volume"), c.FormValue("dest"), c.FormValue("method"), "")
	return h.volume(c, "backup queued", err)
}

func (h *handler) Restore(c echo.Context) error {
	id := c.Param("volume")
	_, err := h.orch.RestoreBackup(c.Request().Context(), c.QueryParam("run"), id, id)
	return h.volume(c, "restore queued", err)
}

func (h *handler) DeleteVolume(c echo.Context) error {
	err := h.orch.DeleteVolume(c.Request().Context(), c.Param("volume"))
	if err == nil {
		return respond.HTML(c, http.StatusOK, volume.Deleted())
	}
	return h.volume(c, "", err)
}

// volume is the backups tab: the volume, its schedules and runs, and what
// a backup can go to and with.
func (h *handler) volume(c echo.Context, note string, actErr error) error {
	msg, status, fail := refused(actErr)
	if fail != nil {
		return fail
	}
	ctx, id := c.Request().Context(), c.Param("volume")
	vol, err := h.orch.Volume(ctx, id)
	if err != nil {
		return middleware.HTTPError(err)
	}
	scheds, err := h.orch.BackupSchedules(ctx, id)
	if err != nil {
		return middleware.HTTPError(err)
	}
	runs, err := h.orch.BackupRuns(ctx, id)
	if err != nil {
		return middleware.HTTPError(err)
	}
	dests, err := h.orch.BackupDests(ctx, scope(c).Org.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	methods, err := h.orch.BackupMethods(ctx, id)
	if err != nil {
		return middleware.HTTPError(err)
	}
	v := volume.View{Name: vol.Name, Scope: vol.ScopeKind, Orphaned: when(vol.OrphanedAt), Methods: methods,
		Base: render.EnvURL(c) + "/-/volumes/" + id, Error: msg,
		Dests: []volume.Option{{Value: "", Label: "local disk"}}}
	if msg == "" {
		v.Note = note
	}
	names := map[string]string{"": "local disk"}
	for _, d := range dests {
		names[d.ID] = d.Name
		v.Dests = append(v.Dests, volume.Option{Value: d.ID, Label: d.Name})
	}
	for _, s := range scheds {
		dest := ""
		if s.DestID != nil {
			dest = *s.DestID
		}
		v.Schedules = append(v.Schedules, volume.Schedule{Method: s.Method, Cron: s.Cron, Dest: names[dest], Keep: fmt.Sprint(s.Keep)})
	}
	for _, r := range runs {
		v.Runs = append(v.Runs, volume.Run{ID: r.ID, Status: r.Status, Trigger: r.Trigger, When: when(&r.CreatedAt),
			Size: size(r.SizeBytes), Error: r.Error, Restorable: r.Status == "done" && r.ObjectKey != ""})
	}
	return respond.HTML(c, status, volume.Backups(v))
}

func size(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%d KB", b>>10)
}
