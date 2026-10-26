// Package admin serves the Server settings page under /-/admin/<section>,
// its actions, the node drawer that opens over it, and the panel jobs'
// stream at /-/jobs/:job/events. Every admin verb is admin-level, so the
// route's Require is the whole gate.
package admin

import (
	"cmp"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/FyrmForge/hamr/pkg/htmx"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler { return &handler{orch: orch} }

func (h *handler) Mount(g *echo.Group, a *middleware.Access) {
	b := ui.Base
	g.GET(b+"/:section", h.Section, a.Require("admin.read"))
	// the page's own root is its first section
	overview := func(c echo.Context) error { return c.Redirect(http.StatusSeeOther, ui.Href("overview")) }
	g.GET(b, overview, a.Require("admin.read"))
	g.GET(b+"/", overview, a.Require("admin.read"))
	g.GET(
		"/-/jobs/:job/events",
		func(c echo.Context) error { return render.JobStream(c, h.orch, "") },
		a.Require("admin.read"),
	)
	g.POST(b+"/settings/:form", h.SaveSettings, a.Require("serverdefaults.set"))
	h.mountParams(g, a)
	h.mountAccess(g, a)
	g.POST(b+"/users/:user/admin", h.act("users", func(c echo.Context) (string, *service.Job, error) {
		return "Saved.", nil, h.orch.SetAdmin(c.Request().Context(), c.Param("user"), c.QueryParam("admin") == "true")
	}), a.Require("user.admin"))
	g.POST(b+"/users/:user/disable", h.act("users", func(c echo.Context) (string, *service.Job, error) {
		return "User disabled.", nil, h.orch.DisableUser(c.Request().Context(), c.Param("user"))
	}), a.Require("user.admin"))
	g.POST(b+"/users/:user/enable", h.act("users", func(c echo.Context) (string, *service.Job, error) {
		return "User enabled.", nil, h.orch.EnableUser(c.Request().Context(), c.Param("user"))
	}), a.Require("user.admin"))
	g.POST(b+"/update/check", h.Check, a.Require("admin.read"))
	g.POST(b+"/update/run", h.Upgrade, a.Require("container.admin"))
	g.POST(b+"/update/workers", h.act("update", func(c echo.Context) (string, *service.Job, error) {
		j, err := h.orch.UpdateWorkers(c.Request().Context(), middleware.Principal(c))
		return "Worker rollout queued.", &j, err
	}), a.Require("container.admin"))
	g.POST(b+"/caddy", h.act("domains", func(c echo.Context) (string, *service.Job, error) {
		return "Saved.", nil, h.orch.SetSetting(c.Request().Context(), "proxy_custom", c.FormValue("proxy_custom"))
	}), a.Require("proxy.admin"))
	g.POST(b+"/caddy/sync", h.act("domains", func(c echo.Context) (string, *service.Job, error) {
		return "Proxy config re-pushed.", nil, h.orch.SyncProxy(c.Request().Context())
	}), a.Require("proxy.admin"))
	g.POST(b+"/backups", h.act("backups", func(c echo.Context) (string, *service.Job, error) {
		j, err := h.orch.PanelBackupNow(c.Request().Context(), c.FormValue("dest"))
		return "Backup queued.", &j, err
	}), a.Require("container.admin"))
	g.POST(b+"/backups/schedule", h.act("backups", func(c echo.Context) (string, *service.Job, error) {
		on := "false"
		if c.FormValue("panel_backup_enabled") != "" {
			on = "true"
		}
		err := h.orch.SetSettings(c.Request().Context(), map[string]string{
			"panel_backup_enabled":  on,
			"panel_backup_schedule": c.FormValue("panel_backup_schedule"),
			"panel_backup_keep":     c.FormValue("panel_backup_keep"),
			"panel_backup_dest":     c.FormValue("panel_backup_dest"),
		})
		return "Saved.", nil, err
	}), a.Require("serverdefaults.set"))
	g.POST(b+"/backups/recovery/show", h.ShowRecoveryKey, a.Require("container.admin"))
	g.POST(b+"/backups/recovery/saved", h.SavedRecoveryKey, a.Require("container.admin"))
	g.POST(b+"/backups/recovery/rotate", h.RotateRecoveryKey, a.Require("container.admin"))
	g.POST(b+"/security/master-key/rotate", h.RotateMasterKey, a.Require("container.admin"))
	routes := a.Require("route.admin")
	g.POST(b+"/routes", h.act("traffic", func(c echo.Context) (string, *service.Job, error) {
		f := c.FormValue
		_, err := h.orch.CreateExternalRoute(c.Request().Context(), service.ExternalRouteSpec{
			Host:     f("route_host"),
			Mode:     f("route_mode"),
			Target:   f("route_target"),
			Insecure: f("route_insecure") != "",
		})
		return "Route added.", nil, err
	}), routes)
	g.POST(b+"/routes/:route/delete", h.act("traffic", func(c echo.Context) (string, *service.Job, error) {
		return "Route removed.", nil, h.orch.DeleteExternalRoute(c.Request().Context(), c.Param("route"))
	}), routes)
	h.mountConfig(g, a)
	h.mountAlerts(g, a)
	dests := a.Require("serverdefaults.set")
	g.POST(b+"/dests", h.act("backups", func(c echo.Context) (string, *service.Job, error) {
		f := c.FormValue
		d, err := h.orch.CreateBackupDest(c.Request().Context(), nil, service.BackupDestSpec{
			Name:      f("dest_name"),
			Endpoint:  f("endpoint"),
			Region:    f("region"),
			Bucket:    f("bucket"),
			AccessKey: f("access_key"),
			SecretKey: f("secret_key"),
			Shared:    f("share") == comp.ShareAll,
		})
		c.Set(addedDestKey, d.ID) // the panel backup form picks it
		return "Destination added.", nil, err
	}), dests)
	g.POST(b+"/dests/:dest/share", h.act("backups", h.share), dests)
	h.mountConnectors(g, a)
	h.mountNodes(g, a)
	h.mountRegistry(g, a)
	h.mountObserve(g, a)
	g.POST(b+"/dests/:dest/delete", h.act("backups", func(c echo.Context) (string, *service.Job, error) {
		return "Destination removed.", nil, h.orch.DeleteBackupDest(c.Request().Context(), "", c.Param("dest"))
	}), dests)
}

// addedDestKey carries a destination just added to the Backups render.
const addedDestKey = "admin_added_dest"

// share sets who may use a global destination: share=all offers it to every
// org, anything else keeps it the server's. Blank keys keep the stored ones,
// so no key leaves the service.
func (h *handler) share(c echo.Context) (string, *service.Job, error) {
	ctx := c.Request().Context()
	ds, err := h.orch.GlobalBackupDests(ctx)
	if err != nil {
		return "", nil, err
	}
	i := slices.IndexFunc(ds, func(d service.BackupDest) bool { return d.ID == c.Param("dest") })
	if i < 0 {
		return "", nil, errs.ErrNotFound
	}
	d, all := ds[i], c.FormValue("share") == comp.ShareAll
	_, err = h.orch.UpdateBackupDest(ctx, "", d.ID, service.BackupDestSpec{
		Name:     d.Name,
		Endpoint: d.Endpoint,
		Region:   d.Region,
		Bucket:   d.Bucket,
		Shared:   all,
	})
	if all {
		return "Saved. Every org may use it.", nil, err
	}
	return "Saved. Server only.", nil, err
}

// extra is what an action adds to its section's answer.
type extra struct {
	job   *service.Job
	check *ui.UpdateView
	adder *ui.AddNodeView
	// ingress is the DNS preview of a default ingress switch
	ingress *ui.IngressPreview
	// placements is what a placements answer opens above the list
	placements plOpen
}

// GET /-/admin/:section: the page, or for an htmx request aimed anywhere
// but #main (a filter, a poll, a refresh after a job) the view alone.
func (h *handler) Section(c echo.Context) error {
	key := c.Param("section")
	if _, ok := ui.Find(key); !ok {
		return echo.ErrNotFound
	}
	r := c.Request()
	if htmx.IsHTMX(r) && r.Header.Get(htmx.HeaderTarget) != "main" && r.Header.Get(htmx.HeaderHistoryRestore) != "true" {
		return h.answer(c, key, "", extra{}, nil)
	}
	f, body, err := h.view(c, key, extra{})
	if err != nil {
		return middleware.HTTPError(err)
	}
	return render.Page(c, http.StatusOK, "Server settings", ui.Page(f, body))
}

// act runs one action and answers its section: a refusal over it (422), or
// what it did, with the job it queued.
func (h *handler) act(section string, do func(echo.Context) (string, *service.Job, error)) echo.HandlerFunc {
	return func(c echo.Context) error {
		note, j, err := do(c)
		if err != nil {
			j = nil
		}
		return h.answer(c, section, note, extra{job: j}, err)
	}
}

// POST /-/admin/update/check reaches the release host; a failure is the section's text,
// not an error page (a panel may have no way out).
func (h *handler) Check(c echo.Context) error {
	v := ui.UpdateView{Checked: true}
	var err error
	if v.Latest, v.Newer, err = h.orch.CheckUpgrade(c.Request().Context()); err != nil {
		v.CheckErr = err.Error()
	}
	return h.answer(c, "update", "", extra{check: &v}, nil)
}

// POST /-/admin/update/run queues the upgrade; the section says it is under way
// until the job's stream ends, then reloads.
func (h *handler) Upgrade(c echo.Context) error {
	j, err := h.orch.Upgrade(c.Request().Context(), c.QueryParam("tag"))
	if err != nil {
		return h.answer(c, "update", "", extra{}, err)
	}
	return h.answer(c, "update", "", extra{job: &j, check: &ui.UpdateView{Upgrading: true}}, nil)
}

// answer is the view (side menu and section) with a refusal (422) or a note.
func (h *handler) answer(c echo.Context, section, note string, x extra, actErr error) error {
	status := http.StatusOK
	var msg string
	var ref *comp.Refused
	if actErr != nil {
		var ok bool
		if msg, ok = render.Refused(actErr); !ok {
			return middleware.HTTPError(actErr)
		}
		status, note = http.StatusUnprocessableEntity, ""
		c.Set(refusedKey, true)
		// a refusal about one field sits under it; refill keeps the values
		if bad, ok := errs.IsInvalid(actErr); ok && bad.Field != "" {
			ref = &comp.Refused{Field: bad.Field, Msg: msg, More: render.FieldErrs(actErr)}
			c.SetRequest(c.Request().WithContext(comp.WithRefused(c.Request().Context(), ref)))
		}
	}
	f, body, err := h.view(c, section, x)
	if err != nil {
		return middleware.HTTPError(err)
	}
	f.Note, f.Error, f.Ref = note, msg, ref
	if c.Request().Method == http.MethodPost {
		f.At = c.Request().URL.RequestURI()
	}
	return respond.HTML(c, status, ui.View(f, body))
}

// refusedKey marks an answer to a refused act (refill).
const refusedKey = "admin_refused"

// refill is what a refused POST to action carried, for its form to draw
// again as typed; nil on any other answer, so every other form shows what
// is stored. A box left unticked posts nothing: absent is off.
func refill(c echo.Context, action string) url.Values {
	r := c.Request()
	if refused, _ := c.Get(refusedKey).(bool); !refused || r.Method != http.MethodPost || r.URL.Path != action {
		return nil
	}
	f, err := c.FormParams()
	if err != nil {
		return nil
	}
	return f
}

// view is the frame (with the side menu's badges) and the section's body.
func (h *handler) view(c echo.Context, section string, x extra) (ui.ViewFrame, templ.Component, error) {
	ps, err := h.orch.Problems(c.Request().Context())
	if err != nil {
		return ui.ViewFrame{}, nil, err
	}
	f := ui.ViewFrame{Section: section, Badges: badges(ps)}
	// the rail's waiting badge, on the section that answers it
	if n, err := h.orch.HostGrantsWaiting(c.Request().Context()); err == nil && n > 0 {
		f.Badges["host-access"] = ui.Badge{N: n}
	}
	body, err := h.section(c, section, x, ps)
	return f, body, err
}

// problemSection is the section a problem is fixed in: its link's, or Nodes
// for a node drawer; "" = elsewhere (an app).
func problemSection(p service.Problem) string {
	if strings.HasPrefix(p.Link, "?drawer=node:") {
		return "nodes"
	}
	if s, ok := strings.CutPrefix(p.Link, ui.Base+"/"); ok {
		return s
	}
	return ""
}

// badges counts the open problems per section, and every one on Overview.
func badges(ps []service.Problem) map[string]ui.Badge {
	out := map[string]ui.Badge{}
	add := func(key string, critical bool) {
		b := out[key]
		b.N++
		b.Critical = b.Critical || critical
		out[key] = b
	}
	for _, p := range ps {
		crit := p.Severity == service.SevCritical
		add("overview", crit)
		if s := problemSection(p); s != "" && s != "overview" {
			add(s, crit)
		}
	}
	return out
}

func (h *handler) section(c echo.Context, section string, x extra, ps []service.Problem) (templ.Component, error) {
	ctx := c.Request().Context()
	var job *comp.JobStatusView
	if x.job != nil {
		v := render.JobView("", *x.job)
		v.Target = ui.Target
		job = &v
	}
	switch section {
	case "overview":
		return h.overview(c, ps)
	case "users":
		us, err := h.orch.Users(ctx)
		var v ui.UsersView
		for _, u := range us {
			r := ui.User{
				Email:   u.Email,
				Name:    u.Name,
				Admin:   u.Admin(),
				Active:  u.Active,
				Promote: ui.Base + "/users/" + u.ID + "/admin?admin=" + url.QueryEscape(boolWord(!u.Admin())),
			}
			// v0: an admin is never disabled, remove the admin first
			if u.Active && !u.Admin() {
				r.Disable = ui.Base + "/users/" + u.ID + "/disable"
			}
			if !u.Active {
				r.Enable = ui.Base + "/users/" + u.ID + "/enable"
			}
			v.Rows = append(v.Rows, r)
		}
		return ui.Users(v), err
	case "host-access":
		return h.accessTab(c)
	case "nodes":
		return h.nodesTab(c, x)
	case "placements":
		return h.placementsTab(c, x.placements)
	case "traffic":
		return h.trafficTab(c, x)
	case "domains":
		s, err := h.settings(c, "domains", nil)
		if err != nil {
			return nil, err
		}
		val, err := h.orch.Setting(ctx, "proxy_custom")
		if f := refill(c, ui.Base+"/caddy"); f != nil {
			val = f.Get("proxy_custom")
		}
		return ui.Domains(ui.DomainsView{Settings: s, Caddy: ui.CaddyView{Action: ui.Base + "/caddy", Sync: ui.Base + "/caddy/sync", Value: val}}), err
	case "connectors":
		return h.connectorsTab(c)
	case "backups":
		return h.backupsTab(c, job)
	case "registry":
		return h.registryTab(c, job)
	case "cleanup":
		s, err := h.settings(c, "cleanup", nil)
		return ui.Knobs("", s), err
	case "metrics":
		return h.observeTab(c)
	case "alerts":
		return h.alertsTab(c)
	case "keys":
		return h.keysTab(c)
	case "security-log":
		return h.securityTab(c)
	case "org-defaults":
		s, err := h.settings(c, "org-defaults", nil)
		return ui.Knobs("", s), err
	case "jobs":
		s, err := h.settings(c, "jobs", nil)
		return ui.Knobs("", s), err
	case "config":
		return h.configTab(c, job)
	case "update":
		return h.updateTab(c, x, job)
	}
	return nil, errs.ErrNotFound
}

// overview is the open problems, the version and update state, the nodes,
// whether alerts go anywhere, and the setup guide.
func (h *handler) overview(c echo.Context, ps []service.Problem) (templ.Component, error) {
	ctx := c.Request().Context()
	v := ui.OverviewView{Version: h.orch.Version(), Guide: "/-/setup/server/ui/domains"}
	for _, p := range ps {
		r := ui.ProblemRow{Message: p.Message, Href: p.Link, LinkText: p.LinkText, Critical: p.Severity == service.SevCritical, Since: p.Since}
		if it, ok := ui.Find(problemSection(p)); ok {
			r.Area = it.Label
		}
		v.Problems = append(v.Problems, r)
	}
	switch latest, newer, checked := h.orch.LastUpgradeCheck(ctx); {
	case checked && newer:
		v.Update = latest + " available"
	case checked && latest != "":
		v.Update = "up to date"
	}
	var err error
	if v.Panel, err = h.orch.Setting(ctx, "panel_domain"); err != nil {
		return nil, err
	}
	ns, err := h.orch.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	v.Nodes = len(ns) // an offline one is an open problem above
	a, err := h.orch.AlertSettings(ctx)
	v.Webhook = a.URL != ""
	return ui.Overview(v), err
}

// updateTab is the running version, the upgrade and the worker rollout.
func (h *handler) updateTab(c echo.Context, x extra, job *comp.JobStatusView) (templ.Component, error) {
	ctx := c.Request().Context()
	v := ui.UpdateView{}
	if x.check != nil {
		v = *x.check
	} else {
		v.Latest, v.Newer, v.Checked = h.orch.LastUpgradeCheck(ctx)
	}
	v.Version, v.Job = h.orch.Version(), job
	v.Upgrading = v.Upgrading || h.orch.UpgradeUnderway(ctx)
	if job != nil {
		job.Refresh = ui.Href("update")
	}
	if v.Newer && !v.Upgrading {
		v.Run = comp.ConfirmView{
			Button:  "Upgrade to " + v.Latest,
			Title:   "Upgrade stackr to " + v.Latest,
			Warning: "The panel archives its database, then restarts on the new image. Running tiles keep running.",
			Action:  ui.Base + "/update/run?tag=" + url.QueryEscape(v.Latest),
			Target:  ui.Target,
		}
	}
	w, err := h.workersView(c)
	if w != nil && v.Upgrading {
		w.Action = ""
	}
	v.Workers = w
	// A live job stream refreshes the section when it ends; otherwise the
	// section polls itself while the upgrade or the rollout moves, through
	// the panel's restart.
	moving := v.Upgrading || (w != nil && (w.Running || w.Pending))
	if moving && (job == nil || !job.Live) {
		v.Live = ui.Href("update")
	}
	return ui.Update(v), err
}

// backupsTab is the panel's own backup (schedule, last run, back up now),
// the destinations and the history.
func (h *handler) backupsTab(c echo.Context, job *comp.JobStatusView) (templ.Component, error) {
	ctx := c.Request().Context()
	ds, err := h.orch.GlobalBackupDests(ctx)
	if err != nil {
		return nil, err
	}
	rs, err := h.orch.PanelBackups(ctx)
	if err != nil {
		return nil, err
	}
	v := ui.BackupsView{Dests: destsView(ds), Now: ui.Base + "/backups", Job: job}
	if job != nil {
		job.Refresh = ui.Href("backups")
	}
	v.Sched.Action = ui.Base + "/backups/schedule"
	for _, k := range []struct {
		key string
		to  *string
	}{{"panel_backup_schedule", &v.Sched.Schedule}, {"panel_backup_keep", &v.Sched.Keep}, {"panel_backup_dest", &v.Sched.Dest}} {
		if *k.to, err = h.orch.Setting(ctx, k.key); err != nil {
			return nil, err
		}
	}
	on, err := h.orch.Setting(ctx, "panel_backup_enabled")
	if err != nil {
		return nil, err
	}
	v.Sched.Enabled = on == "true"
	if f := refill(c, v.Sched.Action); f != nil {
		v.Sched.Schedule, v.Sched.Keep, v.Sched.Dest = f.Get("panel_backup_schedule"), f.Get("panel_backup_keep"), f.Get("panel_backup_dest")
		v.Sched.Enabled = f.Get("panel_backup_enabled") != ""
	}
	if f := refill(c, v.Dests.Add); f != nil {
		v.Dests.Draft = &comp.DestDraft{
			Name: f.Get("dest_name"), Endpoint: f.Get("endpoint"), Bucket: f.Get("bucket"),
			Region: f.Get("region"), AccessKey: f.Get("access_key"), Shared: f.Get("share") == comp.ShareAll,
		}
	}
	off, err := h.orch.PanelBackupOffBox(ctx)
	if err != nil {
		return nil, err
	}
	v.NoOffBox = !off
	v.RecoveryGen, _ = h.orch.RecoveryStatus()
	if added, _ := c.Get(addedDestKey).(string); v.Sched.Dest == "" {
		v.Sched.Dest = added
	}
	destName := map[string]string{}
	for _, d := range ds {
		destName[d.ID] = d.Name
		if d.Kind != "local" {
			v.Sched.Dests = append(v.Sched.Dests, ui.DestOption{Value: d.ID, Label: d.Name})
		}
	}
	for _, r := range rs {
		v.Rows = append(v.Rows, ui.Backup{
			Status:  r.Status,
			Trigger: r.Trigger,
			When:    r.CreatedAt,
			Size:    render.Size(r.SizeBytes),
			Error:   r.Error,
			Dest:    cmp.Or(destName[r.DestID], "a removed destination"),
		})
	}
	return ui.Backups(v), nil
}

// destsView is the install's own destinations; the local one is part of
// the install, so it has no actions.
func destsView(ds []service.BackupDest) comp.DestsView {
	v := comp.DestsView{
		Scope:    "instance",
		Add:      ui.Base + "/dests",
		Share:    true,
		ListHelp: "Where backups go. Orgs can add their own too. The panel's own backup may use any of them.",
		Target:   ui.Target,
	}
	for _, d := range ds {
		r := comp.DestView{Name: d.Name, Bucket: d.Bucket, Endpoint: d.Endpoint, Shared: d.Shared}
		if d.Kind == "local" {
			r.Note = "local disk"
			v.Rows = append(v.Rows, r)
			continue
		}
		r.ShareAction = ui.Base + "/dests/" + d.ID + "/share"
		r.Remove = comp.ConfirmView{
			Button:  "Remove",
			Title:   "Remove " + d.Name + "?",
			Warning: "stackr forgets this destination. Archives already in the bucket stay there.",
			Action:  ui.Base + "/dests/" + d.ID + "/delete",
			Target:  ui.Target,
		}
		v.Rows = append(v.Rows, r)
	}
	return v
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// knobForms is each section's plain server knobs, in the order its form
// shows them. Every other server knob has an editor of its own (ownEditor),
// so each knob is edited in exactly one place.
var knobForms = map[string][]string{
	"domains":      {"panel_domain", "root_domain", "acme_email", "trusted_proxies", "ipv6"},
	"registry":     {"registry_cleanup_schedule", "registry_keep"},
	"cleanup":      {"image_check_interval", "cleanup_enabled", "cleanup_schedule", "orphans_enabled", "orphans_schedule", "orphan_retention_days"},
	"org-defaults": {"cpu_limit", "mem_limit_mb", "protect", "protect_user", "protect_password"},
	"jobs":         {"workers", "backup_run_concurrency", "backup_restore_concurrency"},
}

// ownEditor is the server knobs a section edits with a form of its own:
// the custom Caddy config (Domains), the panel backup (Backups) and
// Metrics and logs.
var ownEditor = []string{
	"proxy_custom",
	"panel_backup_enabled", "panel_backup_schedule", "panel_backup_keep", "panel_backup_dest",
	"observe_enabled", "observe_metrics_retention", "observe_min_free", "observe_max_memory",
	"observe_logs_enabled", "observe_logs_retention", "observe_logs_max_size",
	"observe_access_query", "observe_access_ip", "observe_access_headers",
}

// settings is one section's knobs as a settings form; refused marks the
// row it names, or the form.
func (h *handler) settings(c echo.Context, form string, refused error) (comp.SettingsFormView, error) {
	ss, err := h.orch.ServerSettings(c.Request().Context())
	v := comp.SettingsFormView{
		ID: "admin-" + form, Action: ui.Base + "/settings/" + form, Scope: "Server",
		Flat: true, Keys: knobForms[form],
	}
	bad, invalid := errs.IsInvalid(refused)
	marked := false
	if refused != nil {
		c.Set(refusedKey, true)
	}
	posted := refill(c, v.Action)
	for _, s := range ss {
		if !slices.Contains(v.Keys, s.Key) {
			continue
		}
		r := comp.SettingRowView{
			Key:       s.Key,
			Label:     s.Label,
			Desc:      s.Desc,
			Type:      string(s.Type),
			Value:     s.Value,
			Effective: s.Effective,
			DecidedBy: s.DecidedBy,
			Secret:    s.Secret,
			ReadOnly:  s.ReadOnly,
			NoInherit: s.DecidedBy == "install",
		}
		if vs, ok := posted[s.Key]; ok && !s.Secret && !s.ReadOnly {
			r.Value = vs[0]
		}
		if invalid && bad.Field == s.Key {
			r.Error, marked = bad.Msg, true
		}
		v.Rows = append(v.Rows, r)
	}
	if refused != nil && !marked {
		v.Error, _ = render.Refused(refused)
	}
	return v, err
}

// POST /-/admin/settings/:form: the form's own knobs only; the service
// writes the changed ones.
func (h *handler) SaveSettings(c echo.Context) error {
	form := c.Param("form")
	keys, ok := knobForms[form]
	if !ok {
		return echo.ErrNotFound
	}
	f, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	vals := map[string]string{}
	for _, k := range keys {
		if vs := f[k]; len(vs) > 0 {
			vals[k] = vs[0]
		}
	}
	actErr := h.orch.SetServerSettings(c.Request().Context(), vals)
	if _, ok := render.Refused(actErr); actErr != nil && !ok {
		return middleware.HTTPError(actErr)
	}
	v, err := h.settings(c, form, actErr)
	if err != nil {
		return middleware.HTTPError(err)
	}
	status := http.StatusOK
	switch {
	case actErr != nil:
		status = http.StatusUnprocessableEntity
	case form == "org-defaults":
		v.Note = "Saved. A changed limit or protect setting redeploys every running tile."
	default:
		v.Note = "Saved."
	}
	return respond.HTML(c, status, comp.SettingsForm(v))
}

// workersView is the Update section's worker rows, with Update workers while any
// worker is behind and no rollout runs, and Retry after a stop. nil = no workers.
func (h *handler) workersView(c echo.Context) (*ui.WorkersView, error) {
	r, err := h.orch.WorkerRollout(c.Request().Context())
	if err != nil || len(r.Nodes) == 0 {
		return nil, err
	}
	w := &ui.WorkersView{Stopped: r.Stopped, Running: r.Running, Pending: r.Pending}
	behind := false
	for _, n := range r.Nodes {
		w.Rows = append(w.Rows, ui.WorkerRow{Name: n.Name, Current: n.Current, State: n.State, Error: n.Error, Behind: n.Behind})
		behind = behind || n.Behind
	}
	if (behind || r.Stopped != "") && !r.Running && !r.Pending {
		w.Action, w.Button = ui.Base+"/update/workers", "Update workers"
		if r.Stopped != "" {
			w.Button = "Retry"
		}
	}
	return w, nil
}
