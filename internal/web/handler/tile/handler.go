// Package tile serves the tile drawer: GET a tab (?tab=), POST an action
// (one verb, then the tab it belongs to again), and the drawer's log
// stream (a replica's or a run's).
package tile

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/ui/access"
	clusterui "github.com/FyrmForge/stackr/internal/ui/cluster"
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
	g.GET(d+"/metrics/events", h.MetricsEvents, read)
	for verb, f := range map[string]func(context.Context, string) (service.Job, error){
		"deploy":  h.orch.Deploy,
		"restart": h.orch.RestartTile,
		"stop":    h.orch.StopTile,
		"start":   h.orch.StartTile,
	} {
		g.POST(d+"/"+verb, h.tileJob(verb, f), write)
	}
	g.POST(d+"/delete", h.Delete, write)
	g.POST(d+"/jobs/:job/cancel", h.CancelJob, require("deployment.cancel"))
	g.POST(d+"/run", h.Run, write)
	g.POST(d+"/pause", h.Pause, write)
	g.POST(d+"/runs/:run/stop", h.StopRun, write)
	g.POST(d+"/domains", h.AttachDomain, require("domain.write"))
	g.POST(d+"/domains/:domain/detach", h.DetachDomain, require("domain.write"))
	g.POST(d+"/domains/:domain/raw", h.SetRawCaddy, require("proxy.admin"))
	g.POST(d+"/env", h.SetEnv, write)
	g.POST(d+"/access", h.SetAccess, write)
	g.POST(d+"/settings", h.SetSettings, write)
	g.POST(d+"/rename", h.Rename, write)
	g.POST(d+"/image/check", h.CheckImage, write)
}

func tileOf(c echo.Context) *service.Tile {
	return middleware.ScopeOf(c).Tile
}

func base(c echo.Context) string {
	return render.EnvURL(c) + "/-/tiles/" + tileOf(c).Slug
}

// canShell is whether the viewer may open a shell: the verb the terminal
// endpoint itself needs.
func canShell(c echo.Context, s service.Scope) bool {
	p := middleware.Principal(c)
	return p != nil && authz.Can(p.Access, "tile.write", authz.Resource{OrgID: s.Org.ID}) == nil
}

// can is whether the viewer holds verb in this org.
func can(c echo.Context, verb string) bool {
	p := middleware.Principal(c)
	return p != nil && authz.Can(p.Access, authz.Verb(verb), authz.Resource{OrgID: middleware.ScopeOf(c).Org.ID}) == nil
}

// head is what every answer's header reads, once per request.
type head struct {
	status service.TileStatus
	image  service.Image // a pulled tile's watch; zero otherwise
}

// typed is a refused domain add, drawn over the stored row.
type typed struct {
	replace *ui.ReplaceOffer // the Settings tab asks "Replace it"
	noIP    *ui.NoIPRefusal  // the ingress node has no public IP
	kept    map[string]string // a refused add's form, drawn again
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
		Tab:      ui.Tab(t.Kind, tab, canShell(c, s), !can(c, "tile.write")),
		Terminal: canShell(c, s),
		ReadOnly: !can(c, "tile.write"),
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
	if ok, err := render.ToShell(c, tileOf(c).ID); ok {
		return err
	}
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
		g, err := h.orch.HostGrant(ctx, t.StackID)
		if err != nil {
			return middleware.HTTPError(err)
		}
		sv.Access = access.ForTile(g.Lines, g.Pending, t.Slug)
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
		lv := logsView(c, v.Base, *t, hd.status, last)
		if h.orch.LogsOn() {
			lv.History = render.EnvURL(c) + "/-/logs?from=tile&tile=" + url.QueryEscape(t.Slug)
		}
		body = ui.Logs(v, lv)
	case "terminal":
		body = ui.Terminal(v, terminalView(c, hd.status))
	case "metrics":
		r := comp.MetricRange(c.QueryParam("range"))
		in := service.MetricsIn{}
		in.Range, _ = time.ParseDuration(r)
		m, err := h.orch.TileMetrics(ctx, t.ID, in)
		if err != nil {
			return middleware.HTTPError(err)
		}
		ds, err := h.orch.Domains(ctx, t.ID)
		if err != nil {
			return middleware.HTTPError(err)
		}
		v.Domained = len(ds) > 0
		body = ui.Metrics(v, render.MetricsLive(h.orch, m, r, v.Base+"?tab=metrics", v.Base+"/metrics/events"))
	case "settings":
		sv, err := h.settings(c, *t, hd)
		if err != nil {
			return middleware.HTTPError(err)
		}
		if ty != nil {
			if sv.Domains != nil {
				sv.Domains.Replace, sv.Domains.NoIP, sv.Domains.Kept = ty.replace, ty.noIP, ty.kept
			}
		}
		body = ui.Settings(v, sv)
	case "jobs":
		js, err := h.orch.TileJobs(ctx, []string{t.ID}, 20)
		if err != nil {
			return middleware.HTTPError(err)
		}
		cancel := ""
		if can(c, "deployment.cancel") {
			cancel = v.Base
		}
		body = ui.Jobs(v, jobsView(cancel, js))
	case "runs":
		rs, err := h.orch.Runs(ctx, t.ID, 20)
		if err != nil {
			return middleware.HTTPError(err)
		}
		body = ui.Runs(v, runsView(v.Base, hd.status, rs))
	}
	return respond.HTML(c, status, ui.Drawer(v, body))
}

// GET …/-/tiles/:tile/metrics/events: the Metrics tab's new points
// (tile.read, as the tab).
func (h *handler) MetricsEvents(c echo.Context) error {
	s, err := h.orch.TileScope(c.Request().Context(), tileOf(c).ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return render.MetricsEvents(c, h.orch, s)
}

// settings is the Settings tab: the rows over the stored tile, the limits
// it inherits, its variables, database access, volumes and ingress, the
// domains of a kind with an endpoint, a pulled image's watch. ?open= (or
// a row action's) is the row open.
func (h *handler) settings(c echo.Context, t service.Tile, hd head) (ui.SettingsView, error) {
	ctx, s := c.Request().Context(), middleware.ScopeOf(c)
	sv := settingsView(t)
	sv.Open = c.FormValue("open")
	sv.Env = envView(t.EnvJSON)
	server, err := h.orch.SettingDefaults(ctx)
	if err != nil {
		return sv, err
	}
	if err := inherit(&sv, []rung{{"env", s.Env.Settings}, {"stack", s.Stack.Settings}, {"org", s.Org.Settings}, {"server", server.JSON()}}); err != nil {
		return sv, err
	}
	if sv.Access, err = h.access(c, t); err != nil {
		return sv, err
	}
	vs, err := h.orch.TileVolumes(ctx, t.ID)
	if err != nil {
		return sv, err
	}
	sv.Volumes = volumeRows(render.EnvURL(c), vs)
	pv, err := h.orch.Placement(ctx, service.ScopeTile, t.ID)
	if err != nil {
		return sv, err
	}
	sv.Placement = base(c) + "/placement"
	sv.OwnExits = !pv.Inherited && len(pv.Rungs) > 0
	sv.PlacementSet = sv.OwnExits || pv.Ingress != ""
	if !runKind(t.Kind) {
		sv.Ingress = ingressWord(pv.IngressName, pv.Ingress, pv.IngressFrom)
		ds, err := h.orch.Domains(ctx, t.ID)
		if err != nil {
			return sv, err
		}
		managed, err := h.orch.TileDNSDomains(ctx, t.ID)
		if err != nil {
			return sv, err
		}
		auto, err := h.orch.TileAutoHost(ctx, t.ID)
		if err != nil {
			return sv, err
		}
		p := middleware.Principal(c)
		sv.Domains = &ui.DomainsView{Rows: domainRows(ds), Admin: p != nil && p.Access.Admin, Managed: managed, AutoHost: auto}
		if t.ContainerPort != 0 {
			sv.Domains.Port = strconv.Itoa(t.ContainerPort)
		}
	}
	if pulls(t) {
		iv := imageView(t, hd.image)
		sv.Image = &iv
	}
	return sv, nil
}

// access is the Database access row: the tile's slice_access entries, the creds
// it holds, and the env's slice tiles, each linked to its drawer.
func (h *handler) access(c echo.Context, t service.Tile) (ui.AccessView, error) {
	ctx := c.Request().Context()
	env := render.EnvURL(c)
	var a ui.AccessView
	ts, err := h.orch.Tiles(ctx, t.EnvironmentID)
	if err != nil {
		return a, err
	}
	ids := map[string]string{} // slice slug → tile id
	for _, x := range ts {
		if x.Kind == "slice" {
			ids[x.Slug] = x.ID
			a.Slices = append(a.Slices, x.Slug)
		}
	}
	link := func(id string) string {
		if id == "" {
			return ""
		}
		return env + "?drawer=" + id + "&tab=overview"
	}
	for _, e := range t.SliceAccess {
		a.Entries = append(a.Entries, ui.AccessEntry{
			Slice:  e.From,
			Link:   link(ids[e.From]),
			Access: e.Access,
		})
		a.Slices = slices.DeleteFunc(a.Slices, func(s string) bool { return s == e.From }) // the add form offers the rest
	}
	bs, err := h.orch.ConsumerBindings(ctx, t.ID)
	if err != nil {
		return a, err
	}
	for _, b := range bs {
		a.Bindings = append(a.Bindings, ui.AccessBinding{
			Slice:  b.Slice,
			Link:   link(b.SliceID),
			Access: b.Access,
			User:   b.User,
			Since:  b.Since,
		})
	}
	return a, nil
}

// after answers an action with its tab: a refusal shows over it (422), a
// done action says what it did.
func (h *handler) after(c echo.Context, tab, note string, err error) error {
	v, hd, verr := h.view(c, tab)
	if verr != nil {
		return middleware.HTTPError(verr)
	}
	if err != nil {
		msg, ok := render.Refuse(c, c.FormValue("open"), err) // a settings row takes its own refusal
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

// Delete queues the tile's removal and goes back to its env's canvas: the
// drawer would poll a tile that is about to be gone (a 404).
func (h *handler) Delete(c echo.Context) error {
	t := tileOf(c)
	if _, err := h.orch.DeleteTile(c.Request().Context(), t.ID); err != nil {
		return h.after(c, "settings", "", err)
	}
	hamrmw.SetFlash(c, "Deleting "+t.Name+".", hamrmw.FlashInfo)
	c.Response().Header().Set("HX-Redirect", render.EnvURL(c))
	return c.NoContent(http.StatusOK)
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

// CancelJob cancels one of this tile's own jobs, then shows the Deployments tab.
func (h *handler) CancelJob(c echo.Context) error {
	ctx := c.Request().Context()
	j, err := h.orch.GetJob(ctx, c.Param("job"))
	if err != nil {
		return middleware.HTTPError(err)
	}
	if !slices.Contains(j.LockSet, tileOf(c).ID) {
		return echo.NewHTTPError(http.StatusNotFound, "no such job")
	}
	return h.after(c, "jobs", "job cancelled", h.orch.CancelJob(ctx, c.Param("job")))
}

func (h *handler) StopRun(c echo.Context) error {
	err := h.orch.StopRun(c.Request().Context(), tileOf(c).ID, c.Param("run"))
	return h.after(c, "runs", "run stopped", err)
}

// AttachDomain takes the literal form, or the auto one: the orchestrator
// names an auto host. A managed domain picked from the list plus its
// subdomain is the host; "Other" (no domain) keeps the typed one. A host
// whose name has a DNS record stackr did not create asks "Replace it".
func (h *handler) AttachDomain(c echo.Context) error {
	if c.FormValue("auto") != "" {
		d, err := h.orch.AttachDomain(c.Request().Context(), tileOf(c).ID, service.DomainSpec{Auto: true})
		return h.after(c, "settings", "Attached "+d.Host+".", autoRefusal(err))
	}
	https := c.FormValue("https") != ""
	host := strings.TrimSpace(c.FormValue("host"))
	if d := c.FormValue("domain"); d != "" {
		host = service.ManagedHost(d, c.FormValue("sub"))
	}
	s := service.DomainSpec{
		Host:       host,
		Path:       c.FormValue("path"),
		RedirectTo: strings.TrimSpace(c.FormValue("redirect_to")),
		HTTPS:      &https,
	}
	if u := strings.TrimSpace(c.FormValue("forward_auth")); u != "" {
		s.Extras.ForwardAuth = &service.ForwardAuth{URL: u}
	}
	if p := c.FormValue("port"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return h.refuseDomain(c, errs.Invalidf("port", "Use a number for the port."))
		}
		s.Port = n
	}
	ctx, org := c.Request().Context(), middleware.ScopeOf(c).Org.ID
	if c.FormValue("adopt") != "" {
		if err := h.orch.AdoptDNSHost(ctx, org, s.Host); err != nil {
			return h.after(c, "settings", "", err)
		}
	}
	_, err := h.orch.AttachDomainReplace(ctx, tileOf(c).ID, s, c.FormValue("replace") != "")
	if errors.As(err, new(*service.RecordTaken)) {
		rows, perr := h.orch.PlanAdoptDNSHost(ctx, org, s.Host)
		if perr != nil {
			return h.after(c, "settings", "", perr)
		}
		return h.offerReplace(c, s.Host, len(rows) > 0)
	}
	if np := new(service.NoPublicIP); errors.As(err, &np) {
		return h.refuseNoIP(c, np)
	}
	if _, ok := render.Refused(err); err != nil && ok {
		return h.refuseDomain(c, err)
	}
	return h.after(c, "settings", "Attached "+s.Host+".", err)
}

// refuseDomain answers a refused add: the add form opens again with what
// was typed, the reason over its row.
func (h *handler) refuseDomain(c echo.Context, err error) error {
	v, hd, verr := h.view(c, "settings")
	if verr != nil {
		return middleware.HTTPError(verr)
	}
	v.Error, _ = render.Refuse(c, c.FormValue("open"), err)
	return h.show(c, http.StatusUnprocessableEntity, v, hd, &typed{kept: domainForm(c)})
}

// refuseNoIP answers an add refused on an ingress node with no public IP:
// the node named, and for a server admin a link to its Network tab.
func (h *handler) refuseNoIP(c echo.Context, np *service.NoPublicIP) error {
	v, hd, err := h.view(c, "settings")
	if err != nil {
		return middleware.HTTPError(err)
	}
	r := &ui.NoIPRefusal{Node: np.Node}
	if p := middleware.Principal(c); p != nil && p.Access.Admin {
		r.Link = clusterui.Base + clusterui.NodePush(np.NodeID, "network")
	}
	return h.show(c, http.StatusUnprocessableEntity, v, hd, &typed{noIP: r})
}

// offerReplace answers an add refused on a record stackr did not create:
// the Settings tab asking whether to replace it, the form kept. When the
// record already points at our node (adopt) the question is whether to let
// stackr manage it, and yes adopts it before the retry.
func (h *handler) offerReplace(c echo.Context, host string, adopt bool) error {
	form := domainForm(c)
	vals := maps.Clone(form)
	vals["replace"] = "1"
	if adopt {
		delete(vals, "replace")
		vals["adopt"] = "1"
	}
	b, err := json.Marshal(vals)
	if err != nil {
		return err
	}
	v, hd, err := h.view(c, "settings")
	if err != nil {
		return middleware.HTTPError(err)
	}
	return h.show(c, http.StatusUnprocessableEntity, v, hd, &typed{replace: &ui.ReplaceOffer{Host: host, Vals: string(b), Form: form, Adopt: adopt}})
}

// domainForm is the add domain form as posted, for drawing it again.
func domainForm(c echo.Context) map[string]string {
	form := map[string]string{}
	for _, k := range []string{"host", "domain", "sub", "path", "port", "redirect_to", "forward_auth", "https"} {
		if v := c.FormValue(k); v != "" {
			form[k] = v
		}
	}
	return form
}

// autoRefusal drops the field off a refused auto attach: its form is one
// button, so no field is there to name. The port is the tile's (v0's line).
func autoRefusal(err error) error {
	bad, invalid := errs.IsInvalid(err)
	switch {
	case !invalid:
		return err
	case bad.Field == "port":
		return errs.Invalid{Msg: "Set a container port on the tile first."}
	}
	return errs.Invalid{Msg: bad.Msg}
}

func (h *handler) DetachDomain(c echo.Context) error {
	err := h.orch.DetachDomain(c.Request().Context(), c.Param("domain"))
	return h.after(c, "settings", "Detached.", err)
}

func (h *handler) SetRawCaddy(c echo.Context) error {
	_, err := h.orch.SetRawCaddy(c.Request().Context(), c.Param("domain"), c.FormValue("raw_caddy"))
	return h.after(c, "settings", "Saved.", err)
}

// update applies edit through UpdateTile and says whether it redeploys.
func (h *handler) update(c echo.Context, tab string, edit func(*service.Tile) error) error {
	t, j, err := h.orch.UpdateTile(c.Request().Context(), tileOf(c).ID, edit)
	note := "Saved."
	if j != nil {
		note = "Saved. A redeploy is queued."
	}
	if err == nil {
		*tileOf(c) = t // the tab shows the edited row, not the scope's
	}
	return h.after(c, tab, note, err)
}

// POST …/env: the editor's KEY=VALUE lines, or one row's × (drop).
func (h *handler) SetEnv(c echo.Context) error {
	drop, text := c.FormValue("drop"), c.FormValue("env")
	return h.update(c, "settings", func(t *service.Tile) error {
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

// POST …/access: slice (a slice tile slug of this env) and access; an
// entry already there is replaced, access=default drops it.
func (h *handler) SetAccess(c echo.Context) error {
	t, err := h.orch.SetSliceAccess(c.Request().Context(), tileOf(c).ID, c.FormValue("slice"), c.FormValue("access"))
	if err == nil {
		*tileOf(c) = t
	}
	return h.after(c, "settings", "Saved.", err)
}

// POST …/rename: the General row's name. The slug and the drawer's URL
// move with it.
func (h *handler) Rename(c echo.Context) error {
	t, warn, err := h.orch.RenameTile(c.Request().Context(), tileOf(c).ID, strings.TrimSpace(c.FormValue("name")))
	if err == nil {
		*tileOf(c) = t
	}
	return h.after(c, "settings", strings.Join(append([]string{"Saved."}, warn...), " "), err)
}

func (h *handler) CheckImage(c echo.Context) error {
	s := middleware.ScopeOf(c)
	_, err := h.orch.CheckImages(c.Request().Context(), s.Stack.ID, s.Tile.ID)
	return h.after(c, "settings", "Check queued.", err)
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
		note := "Saved."
		if j != nil {
			note = "Saved. A redeploy is queued."
		}
		return h.after(c, "settings", note, nil)
	}
	return h.after(c, "settings", "", rowRefusal(err, form.Get("fields")))
}

// rowRefusal names the form field a refused row save is about (Validate
// speaks the stack file's keys); a field the row does not draw (a volume
// refused for the replicas) leaves the reason over the row instead.
func rowRefusal(err error, fields string) error {
	bad, invalid := errs.IsInvalid(err)
	if !invalid {
		return err
	}
	k := formKey(bad.Field)
	if !slices.Contains(strings.Fields(fields), k) {
		k = ""
	}
	return errs.Invalid{Field: k, Msg: bad.Msg}
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
			return comp.NewLogLine(at, msg)
		}
	}
	return comp.NewLogLine(time.Time{}, l)
}
