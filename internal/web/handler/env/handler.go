// Package env is the env canvas's routes that are not the canvas itself:
// the env-level drawers and dialogs (managed instance, slice tile, volume,
// proxy, create tile, rollback) and the drawers' job stream. Every handler is one verb call and a
// view struct.
package env

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/authz"
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

func NewHandler(orch *service.Orchestrator) *handler {
	return &handler{orch: orch}
}

func scope(c echo.Context) service.Scope {
	return middleware.ScopeOf(c)
}

// Mount registers the env's drawers and dialogs on g, under the env
// page's "/-/" (never a slug). The env stream is the canvas's.
func (h *handler) Mount(g *echo.Group, a *middleware.Access) {
	e := "/:org/:stack/:env/-"
	g.GET(e+"/jobs/:job/events", h.JobEvents, a.Require("deployment.read"))
	g.POST(e+"/rollback/:release", h.Rollback, a.Require("env.write"))
	g.GET(e+"/new-tile", h.NewTile, a.Require("tile.write"))
	g.POST(e+"/new-tile", h.CreateTile, a.Require("tile.write"))
	g.GET(e+"/instances/:tile", h.Instance, a.Require("tile.read"))
	g.POST(e+"/instances/:tile/deploy", h.instanceJob("deploy queued", h.orch.Deploy), a.Require("tile.write"))
	g.POST(e+"/instances/:tile/stop", h.instanceJob("stop queued", h.orch.StopTile), a.Require("tile.write"))
	g.POST(e+"/instances/:tile/delete", h.instanceJob("delete queued", h.orch.DeleteTile), a.Require("tile.write"))
	g.POST(e+"/instances/:tile/allow", h.Allow, a.Require("managed.allow"))
	g.POST(e+"/instances/:tile/env-pairs", h.EnvPairs, a.Require("tile.write"))
	g.GET(e+"/slices/:tile", h.Slice, a.Require("tile.read"))
	g.POST(e+"/slices/:tile/default-access", h.SliceDefaultAccess, a.Require("tile.write"))
	g.POST(e+"/slices/:tile/access", h.SliceAccess, a.Require("tile.write"))
	g.POST(e+"/slices/:tile/on-remove", h.SliceOnRemove, a.Require("tile.write"))
	g.POST(e+"/slices/:tile/delete", h.SliceDelete, a.Require("tile.write"))
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

// GET …/-/jobs/:job/events
func (h *handler) JobEvents(c echo.Context) error {
	return render.JobStream(c, h.orch, render.EnvURL(c))
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
	v := dialog.CreateTileView{
		Action:   url,
		Switch:   url,
		Source:   c.FormValue("source"),
		Name:     c.FormValue("name"),
		Image:    c.FormValue("image_ref"),
		GitURL:   c.FormValue("git_url"),
		Branch:   c.FormValue("git_branch"),
		Schedule: c.FormValue("schedule"),
		Trigger:  c.FormValue("trigger"),
		Engine:   c.FormValue("engine"),
		Command:  c.FormValue("command"),
		From:     c.FormValue("provision_from"),
		Access:   c.FormValue("default_access"),
	}
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
	t := service.Tile{
		StackID:       s.Stack.ID,
		EnvironmentID: s.Env.ID,
		Name:          v.Name,
		Kind:          v.Source,
	}
	var err error
	switch v.Source {
	case "managed":
		t, err = h.orch.CreateManagedTile(c.Request().Context(), t, v.Engine)
	case "slice":
		t, err = h.orch.CreateSliceTile(c.Request().Context(), s.Env.ID, v.Name, v.From, v.Access)
	default:
		t.ImageRef = v.Image
		t.GitURL = v.GitURL
		t.GitBranch = v.Branch
		t.Schedule = v.Schedule
		t.Trigger = v.Trigger
		t.Command = v.Command
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
		if field != "general" &&
			!slices.Contains(createFields, field) {
			v.Errors["general"] = field + ": " + msg // a field the form does not show
		}
		return respond.HTML(c, http.StatusUnprocessableEntity, dialog.CreateTile(v))
	}
	tab := "status"
	if t.Kind == "slice" {
		tab = slice.Tabs[0]
	}
	c.Response().Header().Set("HX-Redirect", render.EnvURL(c)+"?drawer="+t.ID+"&tab="+tab)
	return c.NoContent(http.StatusOK)
}

// createFields are the fields the create form marks; another field's
// refusal shows over the form.
var createFields = []string{
	"name",
	"image_ref",
	"git_url",
	"git_branch",
	"schedule",
	"engine",
	"provision_from",
	"default_access",
}

// ---- managed instance ----

// GET …/-/instances/:tile?tab=
func (h *handler) Instance(c echo.Context) error {
	return h.instance(c, c.QueryParam("tab"), "", nil, instance.SettingsView{})
}

// instanceJob is a header action (deploy, stop, delete) answered with
// the instance's Overview.
func (h *handler) instanceJob(note string, f func(context.Context, string) (service.Job, error)) echo.HandlerFunc {
	return func(c echo.Context) error {
		_, err := f(c.Request().Context(), scope(c).Tile.ID)
		return h.instance(c, "slices", note, err, instance.SettingsView{})
	}
}

// POST …/-/instances/:tile/allow: the whole list (allow, repeated) plus
// add, one more pattern. Blank entries are dropped.
func (h *handler) Allow(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	add := strings.TrimSpace(form.Get("add"))
	list := nonBlank(append(form["allow"], add))
	_, err = h.orch.SetManagedAllow(c.Request().Context(), scope(c).Tile.ID, list)
	return h.saved(c, "allow", err, instance.SettingsView{Add: add})
}

// POST …/-/instances/:tile/env-pairs: the whole map (from and to,
// repeated in step) plus add_from → add_to, one more pair.
func (h *handler) EnvPairs(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	from := form["from"]
	to := form["to"]
	if len(from) != len(to) {
		return echo.NewHTTPError(http.StatusBadRequest, "from and to must pair up")
	}
	typed := instance.SettingsView{
		AddFrom: strings.TrimSpace(form.Get("add_from")),
		AddTo:   form.Get("add_to"),
	}
	pairs := map[string]string{}
	for i := range from {
		pairs[from[i]] = to[i]
	}
	if typed.AddFrom != "" {
		pairs[typed.AddFrom] = typed.AddTo
	}
	_, err = h.orch.SetManagedEnvPairs(c.Request().Context(), scope(c).Tile.ID, pairs)
	return h.saved(c, "env_pairs", err, typed)
}

// saved answers a Settings form: a refusal of its own field shows under
// the form with what was typed (422), any other over the drawer.
func (h *handler) saved(c echo.Context, field string, err error, typed instance.SettingsView) error {
	var form instance.SettingsView
	if err != nil {
		form = typed
	}
	if inv, ok := errs.IsInvalid(err); ok && inv.Field == field {
		form.Errors = map[string]string{field: inv.Msg}
		err = nil
	}
	return h.instance(c, "settings", "saved", err, form)
}

func nonBlank(list []string) []string {
	out := []string{}
	for _, s := range list {
		if strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// instance is the managed instance drawer on tab; each tab is one read.
// form is a Settings save's answer: what was typed and why it was refused.
func (h *handler) instance(c echo.Context, tab, note string, actErr error, form instance.SettingsView) error {
	msg, status, fail := refused(actErr)
	if fail != nil {
		return fail
	}
	if len(form.Errors) > 0 {
		status = http.StatusUnprocessableEntity
	}
	ctx, s := c.Request().Context(), scope(c)
	t := s.Tile
	m, ps, err := h.orch.InstanceSlices(ctx, t.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	st, err := h.orch.TileStatus(ctx, t.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	v := instance.View{
		Node:     t.ID,
		Name:     t.Name,
		Engine:   m.Engine,
		Base:     render.EnvURL(c) + "/-/instances/" + t.Slug,
		Tab:      instance.Tab(tab),
		Status:   st.Word,
		Running:  st.Word == "running",
		Location: s.Stack.Name + " / " + s.Env.Name,
		EnvColor: s.Env.Color,
		Error:    msg,
	}
	if msg == "" && len(form.Errors) == 0 {
		v.Note = note
	}
	var body templ.Component
	switch v.Tab {
	case "logs":
		pane, running := h.logPane(c, t.Slug, st)
		body = instance.Logs(v, instance.LogsView{Running: running, Pane: pane})
	case "backups":
		vols, err := h.instanceVolumes(c, t.ID, m.ID)
		if err != nil {
			return middleware.HTTPError(err)
		}
		var bs []volume.BackupsView
		for _, vol := range vols {
			b, err := h.backups(c, vol)
			if err != nil {
				return middleware.HTTPError(err)
			}
			bs = append(bs, b)
		}
		body = instance.Backups(v, bs)
	case "settings":
		body = instance.Settings(v, settingsView(c, m, form))
	default:
		o := instance.OverviewView{
			Endpoint:  m.Endpoint,
			AdminUser: m.AdminUser,
		}
		for _, p := range ps {
			r := instance.SliceRow{
				Slice:    p.Slice,
				Link:     drawerURL(s.Org.Slug, p.Stack, p.Env, p.TileID, slice.Tabs[0]),
				Name:     p.DBName,
				Bindings: p.Bindings,
				Public:   p.Public,
			}
			if p.Stack != s.Stack.Slug || p.Env != s.Env.Slug {
				r.Where = p.Stack + "/" + p.Env
			}
			o.Slices = append(o.Slices, r)
		}
		body = instance.Overview(v, o)
	}
	return respond.HTML(c, status, instance.Drawer(v, body))
}

// settingsView is the instance's allow list and env pairs over what a
// refused save typed.
func settingsView(c echo.Context, m service.ManagedInstance, form instance.SettingsView) instance.SettingsView {
	s := scope(c)
	form.Allow = m.Allow
	form.CanAllow = can(c, s, "managed.allow")
	if form.Add == "" {
		form.Add = s.Org.Slug + ":"
	}
	for _, k := range slices.Sorted(maps.Keys(m.EnvPairs)) {
		form.Pairs = append(form.Pairs, instance.Pair{
			From: k,
			To:   m.EnvPairs[k],
		})
	}
	for _, e := range s.Envs {
		form.Envs = append(form.Envs, e.Slug)
	}
	return form
}

// can asks the one gate about another verb on the scope's org.
func can(c echo.Context, s service.Scope, v authz.Verb) bool {
	p := middleware.Principal(c)
	return p != nil && authz.Can(p.Access, v, authz.Resource{OrgID: s.Org.ID}) == nil
}

// drawerURL is an env canvas with node's drawer open on tab: a link from
// one drawer to another, in this env or any other of the org.
func drawerURL(org, stack, env, node, tab string) string {
	return "/" + org + "/" + stack + "/" + env + "?drawer=" + node + "&tab=" + tab
}

// logPane streams a tile's first container through the tile drawer's
// log route; false = no container yet.
func (h *handler) logPane(c echo.Context, slug string, st service.TileStatus) (comp.LogPaneView, bool) {
	pane := comp.LogPaneView{Level: c.QueryParam("level"), Search: c.QueryParam("q")}
	if len(st.Replicas) == 0 {
		return pane, false
	}
	pane.StreamURL = render.EnvURL(c) + "/-/tiles/" + slug + "/logs/stream?container=" + st.Replicas[0].ID
	return pane, true
}

// instanceVolumes are the volumes an instance mounts or owns, once each.
func (h *handler) instanceVolumes(c echo.Context, tileID, instanceID string) ([]service.Volume, error) {
	ctx := c.Request().Context()
	vols, err := h.orch.TileVolumes(ctx, tileID)
	if err != nil {
		return nil, err
	}
	all, err := h.orch.Volumes(ctx, service.VolumeScope{Kind: "env", ID: scope(c).Env.ID})
	if err != nil {
		return nil, err
	}
	for _, v := range all {
		owned := v.InstanceID != nil && *v.InstanceID == instanceID
		seen := slices.ContainsFunc(vols, func(w service.Volume) bool {
			return w.ID == v.ID
		})
		if owned && !seen {
			vols = append(vols, v)
		}
	}
	return vols, nil
}

// ---- slice tile ----

// GET …/-/slices/:tile
func (h *handler) Slice(c echo.Context) error {
	return h.slice(c, "", nil)
}

// POST …/-/slices/:tile/default-access
func (h *handler) SliceDefaultAccess(c echo.Context) error {
	_, err := h.orch.SetSliceDefaultAccess(c.Request().Context(), scope(c).Tile.ID, c.FormValue("access"))
	return h.slice(c, "saved", err)
}

// POST …/-/slices/:tile/access: consumer (a tile slug of this env) and
// access.
func (h *handler) SliceAccess(c echo.Context) error {
	ctx := c.Request().Context()
	s := scope(c)
	ts, err := h.orch.Tiles(ctx, s.Env.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	i := slices.IndexFunc(ts, func(t service.Tile) bool {
		return t.Slug == c.FormValue("consumer")
	})
	if i < 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "no such consumer in this env")
	}
	_, err = h.orch.SetSliceAccess(ctx, ts[i].ID, s.Tile.Slug, c.FormValue("access"))
	return h.slice(c, "saved", err)
}

// POST …/-/slices/:tile/on-remove
func (h *handler) SliceOnRemove(c echo.Context) error {
	_, err := h.orch.SetSliceOnRemove(c.Request().Context(), scope(c).Tile.ID, c.FormValue("on_remove"))
	return h.slice(c, "saved", err)
}

// POST …/-/slices/:tile/delete
func (h *handler) SliceDelete(c echo.Context) error {
	_, err := h.orch.DeleteTile(c.Request().Context(), scope(c).Tile.ID)
	return h.slice(c, "delete queued", err)
}

// slice is the slice tile drawer: SliceOf and Bindings.
func (h *handler) slice(c echo.Context, note string, actErr error) error {
	msg, status, fail := refused(actErr)
	if fail != nil {
		return fail
	}
	ctx := c.Request().Context()
	s := scope(c)
	t := s.Tile
	sv, err := h.orch.SliceOf(ctx, t.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	bs, err := h.orch.Bindings(ctx, t.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	// A slice has no container: its header word is whether it is provisioned.
	word := "idle"
	if sv.Provisioned {
		word = "provisioned"
	}
	v := slice.View{
		Node:          t.ID,
		Name:          t.Name,
		Base:          render.EnvURL(c) + "/-/slices/" + t.Slug,
		Status:        word,
		Location:      s.Stack.Name + " / " + s.Env.Name,
		EnvColor:      s.Env.Color,
		Error:         msg,
		ProvisionFrom: sv.ProvisionFrom,
		Blocker:       sv.Blocker,
		DefaultAccess: sv.DefaultAccess,
		FileOwned:     s.Stack.ConfigRepo != "",
		OnRemove:      sv.OnRemove,
		Provisioned:   sv.Provisioned,
		DBName:        sv.Name,
		Network:       sv.Network,
	}
	if msg == "" {
		v.Note = note
	}
	// Target is <stack>:<env>:<tile>; a slug never holds a colon.
	if at := strings.Split(sv.Target, ":"); len(at) == 3 {
		v.Target = strings.Join(at, "/")
		v.TargetLink = drawerURL(s.Org.Slug, at[0], at[1], sv.TargetTileID, instance.Tabs[0])
	}
	for _, b := range bs {
		v.Consumers = append(v.Consumers, slice.Consumer{
			Slug:   b.Consumer,
			Link:   render.EnvURL(c) + "?drawer=" + b.ConsumerID + "&tab=access",
			Kind:   b.Kind,
			Access: b.Access,
			User:   b.User,
			Since:  when(&b.Since),
		})
	}
	return respond.HTML(c, status, slice.Drawer(v, slice.Overview(v)))
}

// ---- proxy ----

// GET …/-/proxy
func (h *handler) Proxy(c echo.Context) error {
	rs, err := h.orch.Routes(c.Request().Context(), scope(c).Env.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	var v proxy.View
	for _, r := range rs {
		v.Rows = append(v.Rows, proxy.Route{
			Host:  r.Host,
			Path:  r.Path,
			Tile:  r.Tile,
			Port:  fmt.Sprint(r.ContainerPort),
			HTTPS: r.HTTPS,
			Auto:  r.Auto,
			Raw:   r.RawCaddy != "",
		})
	}
	return respond.HTML(c, http.StatusOK, proxy.Routes(v))
}

// ---- volume ----

// GET …/-/volumes/:volume?tab=
func (h *handler) Volume(c echo.Context) error {
	return h.volume(c, c.QueryParam("tab"), "", nil)
}

func (h *handler) BackupNow(c echo.Context) error {
	_, err := h.orch.BackupNow(c.Request().Context(), c.Param("volume"), c.FormValue("dest"), c.FormValue("method"), "")
	return h.volume(c, "backups", "backup queued", err)
}

func (h *handler) Restore(c echo.Context) error {
	id := c.Param("volume")
	_, err := h.orch.RestoreBackup(c.Request().Context(), c.QueryParam("run"), id, id)
	return h.volume(c, "backups", "restore queued", err)
}

func (h *handler) DeleteVolume(c echo.Context) error {
	err := h.orch.DeleteVolume(c.Request().Context(), c.Param("volume"))
	if err == nil {
		return respond.HTML(c, http.StatusOK, volume.Deleted())
	}
	return h.volume(c, "settings", "", err)
}

// volume is the volume drawer on tab.
func (h *handler) volume(c echo.Context, tab, note string, actErr error) error {
	msg, status, fail := refused(actErr)
	if fail != nil {
		return fail
	}
	ctx, s := c.Request().Context(), scope(c)
	vol, err := h.orch.Volume(ctx, c.Param("volume"))
	if err != nil {
		return middleware.HTTPError(err)
	}
	b, err := h.backups(c, vol)
	if err != nil {
		return middleware.HTTPError(err)
	}
	v := volume.View{
		Node:     vol.ID,
		Name:     vol.Slug,
		Scope:    vol.ScopeKind,
		Base:     b.Base,
		Tab:      volume.Tab(tab),
		Status:   "idle",
		Location: s.Stack.Name + " / " + s.Env.Name,
		EnvColor: s.Env.Color,
		Error:    msg,
	}
	if msg == "" {
		v.Note = note
	}
	if b.Live {
		v.Status = "running"
	}
	b.Poll = polls(c, note, msg)
	var body templ.Component
	switch v.Tab {
	case "backups":
		body = volume.Backups(b)
	case "settings":
		ms, err := h.mounts(c, vol.Slug)
		if err != nil {
			return middleware.HTTPError(err)
		}
		body = volume.Settings(v, blocked(vol, ms))
	default:
		o := volume.OverviewView{
			Docker:   vol.Name,
			Created:  when(&vol.CreatedAt),
			Orphaned: when(vol.OrphanedAt),
		}
		if vol.MaxSizeMB > 0 {
			o.MaxSize = fmt.Sprint(vol.MaxSizeMB, " MB")
		}
		o.Mounts, err = h.mounts(c, vol.Slug)
		if err != nil {
			return middleware.HTTPError(err)
		}
		body = volume.Overview(o)
	}
	return respond.HTML(c, status, volume.Drawer(v, body))
}

// polls is how many times History re-reads after an action queued a
// job: five (10 s) on the action's answer, then what ?poll= counts down.
func polls(c echo.Context, note, msg string) int {
	if note != "" && msg == "" {
		return 5
	}
	n, _ := strconv.Atoi(c.QueryParam("poll")) // absent or bad = no polling
	return min(max(n, 0), 5)
}

// blocked is why a volume cannot be deleted now; "" = it can.
func blocked(vol service.Volume, ms []volume.Mount) string {
	if vol.InstanceID != nil {
		return "Its managed instance owns it. Deleting the instance orphans it; then it can go."
	}
	if len(ms) > 0 {
		return "Detach before deleting: " + ms[0].Tile + " mounts it. The mount lives in that tile's settings."
	}
	return ""
}

// mounts are the env tiles whose volume lines ("slug:/path") name slug.
// ponytail: mirrors the service's unexported mounter check; a verb when a
// second caller needs it.
func (h *handler) mounts(c echo.Context, slug string) ([]volume.Mount, error) {
	ts, err := h.orch.Tiles(c.Request().Context(), scope(c).Env.ID)
	if err != nil {
		return nil, err
	}
	var out []volume.Mount
	for _, t := range ts {
		for _, l := range strings.Split(t.Volumes, "\n") {
			path, ok := strings.CutPrefix(strings.TrimSpace(l), slug+":")
			if ok {
				out = append(out, volume.Mount{Tile: t.Name, Path: path})
			}
		}
	}
	return out, nil
}

// backups is one volume's schedules, runs, and what a backup can go to
// and with.
func (h *handler) backups(c echo.Context, vol service.Volume) (volume.BackupsView, error) {
	ctx := c.Request().Context()
	b := volume.BackupsView{
		ID:    vol.ID,
		Name:  vol.Slug,
		Base:  render.EnvURL(c) + "/-/volumes/" + vol.ID,
		Dests: []volume.Option{{Value: "", Label: "local disk"}},
	}
	scheds, err := h.orch.BackupSchedules(ctx, vol.ID)
	if err != nil {
		return b, err
	}
	runs, err := h.orch.BackupRuns(ctx, vol.ID)
	if err != nil {
		return b, err
	}
	dests, err := h.orch.BackupDests(ctx, scope(c).Org.ID)
	if err != nil {
		return b, err
	}
	b.Methods, err = h.orch.BackupMethods(ctx, vol.ID)
	if err != nil {
		return b, err
	}
	names := map[string]string{"": "local disk"}
	for _, d := range dests {
		names[d.ID] = d.Name
		b.Dests = append(b.Dests, volume.Option{Value: d.ID, Label: d.Name})
	}
	for _, s := range scheds {
		dest := ""
		if s.DestID != nil {
			dest = *s.DestID
		}
		b.Schedules = append(b.Schedules, volume.Schedule{
			Method: s.Method,
			Cron:   s.Cron,
			Dest:   names[dest],
			Keep:   fmt.Sprint(s.Keep),
		})
	}
	for _, r := range runs {
		b.Runs = append(b.Runs, volume.Run{
			ID:         r.ID,
			Status:     r.Status,
			Trigger:    r.Trigger,
			When:       when(&r.CreatedAt),
			Size:       render.Size(r.SizeBytes),
			Error:      r.Error,
			Restorable: r.Status == "done" && r.ObjectKey != "",
		})
		if r.Status == "queued" || r.Status == "running" {
			b.Live = true
		}
	}
	return b, nil
}
