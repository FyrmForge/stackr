// Package admin serves the admin drawer's tabs and actions under /-/admin,
// and the panel jobs' stream at /-/jobs/:job/events. Every admin verb is
// admin-level, so the route's Require is the whole gate.
package admin

import (
	"net/http"
	"net/url"
	"slices"

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
	g.GET(b, h.Drawer, a.Require("admin.read"))
	g.GET(
		"/-/jobs/:job/events",
		func(c echo.Context) error { return render.JobStream(c, h.orch, "") },
		a.Require("admin.read"),
	)
	g.POST(b+"/settings", h.SaveSettings, a.Require("serverdefaults.set"))
	g.POST(b+"/users/:user/admin", h.act("users", func(c echo.Context) (string, *service.Job, error) {
		return "Saved.", nil, h.orch.SetAdmin(c.Request().Context(), c.Param("user"), c.QueryParam("admin") == "true")
	}), a.Require("user.admin"))
	g.POST(b+"/users/:user/disable", h.act("users", func(c echo.Context) (string, *service.Job, error) {
		return "User disabled.", nil, h.orch.DisableUser(c.Request().Context(), c.Param("user"))
	}), a.Require("user.admin"))
	g.POST(b+"/update/check", h.Check, a.Require("admin.read"))
	g.POST(b+"/update/run", h.act("update", func(c echo.Context) (string, *service.Job, error) {
		j, err := h.orch.Upgrade(c.Request().Context(), c.QueryParam("tag"))
		return "Upgrade queued: the panel restarts when it is done.", &j, err
	}), a.Require("container.admin"))
	g.POST(b+"/caddy", h.act("caddy", func(c echo.Context) (string, *service.Job, error) {
		return "Saved.", nil, h.orch.SetSetting(c.Request().Context(), "proxy_custom", c.FormValue("proxy_custom"))
	}), a.Require("proxy.admin"))
	g.POST(b+"/caddy/sync", h.act("caddy", func(c echo.Context) (string, *service.Job, error) {
		return "Proxy config re-pushed.", nil, h.orch.SyncProxy(c.Request().Context())
	}), a.Require("proxy.admin"))
	g.POST(b+"/backups", h.act("backups", func(c echo.Context) (string, *service.Job, error) {
		j, err := h.orch.PanelBackupNow(c.Request().Context())
		return "Backup queued.", &j, err
	}), a.Require("container.admin"))
}

func frame(tab string) comp.DrawerView {
	f := comp.DrawerView{
		Node:  "admin",
		Title: "Admin",
		Kind:  "server",
		Base:  ui.Base,
		Tabs:  ui.Tabs,
		Tab:   tab,
	}
	if !slices.Contains(ui.Tabs, tab) {
		f.Tab = ui.Tabs[0]
	}
	return f
}

// extra is what an action adds to its tab's answer.
type extra struct {
	job   *service.Job
	check *ui.UpdateView
}

// GET /-/admin?tab=
func (h *handler) Drawer(c echo.Context) error {
	return h.answer(c, c.QueryParam("tab"), "", extra{}, nil)
}

// act runs one action and answers its tab: a refusal over it (422), or
// what it did, with the job it queued.
func (h *handler) act(tab string, do func(echo.Context) (string, *service.Job, error)) echo.HandlerFunc {
	return func(c echo.Context) error {
		note, j, err := do(c)
		if err != nil {
			j = nil
		}
		return h.answer(c, tab, note, extra{job: j}, err)
	}
}

// POST /-/admin/update/check reaches GitHub; a failure is the tab's text,
// not an error page (a panel may have no way out).
func (h *handler) Check(c echo.Context) error {
	v := ui.UpdateView{Checked: true}
	var err error
	if v.Latest, v.Newer, err = h.orch.CheckUpgrade(c.Request().Context()); err != nil {
		v.CheckErr = err.Error()
	}
	return h.answer(c, "update", "", extra{check: &v}, nil)
}

func (h *handler) answer(c echo.Context, tab, note string, x extra, actErr error) error {
	f, status := frame(tab), http.StatusOK
	if actErr != nil {
		msg, ok := render.Refused(actErr)
		if !ok {
			return middleware.HTTPError(actErr)
		}
		f.Error, status = msg, http.StatusUnprocessableEntity
	} else {
		f.Note = note
	}
	body, err := h.tab(c, f.Tab, x)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, status, comp.Drawer(f, body))
}

func (h *handler) tab(c echo.Context, tab string, x extra) (templ.Component, error) {
	ctx := c.Request().Context()
	var job *comp.JobStatusView
	if x.job != nil {
		v := render.JobView("", *x.job)
		job = &v
	}
	switch tab {
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
			if u.Active {
				r.Disable = ui.Base + "/users/" + u.ID + "/disable"
			}
			v.Rows = append(v.Rows, r)
		}
		return ui.Users(v), err
	case "update":
		v := ui.UpdateView{}
		if x.check != nil {
			v = *x.check
		}
		v.Version, v.Job = h.orch.Version(), job
		if v.Newer {
			v.Run = comp.ConfirmView{
				Button:  "Upgrade to " + v.Latest,
				Title:   "Upgrade stackr to " + v.Latest,
				Warning: "The panel archives its database, then restarts on the new image. Running tiles keep running.",
				Action:  ui.Base + "/update/run?tag=" + url.QueryEscape(v.Latest),
				Target:  "#" + comp.DrawerRoot,
			}
		}
		return ui.Update(v), nil
	case "caddy":
		val, err := h.orch.Setting(ctx, "proxy_custom")
		return ui.Caddy(ui.CaddyView{Action: ui.Base + "/caddy", Sync: ui.Base + "/caddy/sync", Value: val}), err
	case "backups":
		rs, err := h.orch.PanelBackups(ctx)
		v := ui.BackupsView{Now: ui.Base + "/backups", Job: job}
		for _, r := range rs {
			v.Rows = append(v.Rows, ui.Backup{
				Status:  r.Status,
				Trigger: r.Trigger,
				When:    r.CreatedAt.Local().Format("Jan 2 15:04"),
				Size:    render.Size(r.SizeBytes),
				Error:   r.Error,
			})
		}
		return ui.Backups(v), err
	}
	v, err := h.settings(c, nil)
	return comp.SettingsForm(v), err
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// settings is the server's knobs as the one settings form; refused marks
// the row it names, or the form.
func (h *handler) settings(c echo.Context, refused error) (comp.SettingsFormView, error) {
	ss, err := h.orch.ServerSettings(c.Request().Context())
	v := comp.SettingsFormView{ID: "admin-settings", Action: ui.Base + "/settings", Scope: "Server"}
	bad, invalid := errs.IsInvalid(refused)
	marked := false
	for _, s := range ss {
		r := comp.SettingRowView{
			Key:       s.Key,
			Desc:      s.Desc,
			Type:      string(s.Type),
			Value:     s.Value,
			Effective: s.Effective,
			DecidedBy: s.DecidedBy,
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

// POST /-/admin/settings: every row is posted; the service writes the
// changed ones only.
func (h *handler) SaveSettings(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	vals := map[string]string{}
	for k, vs := range form {
		vals[k] = vs[0]
	}
	actErr := h.orch.SetServerSettings(c.Request().Context(), vals)
	if _, ok := render.Refused(actErr); actErr != nil && !ok {
		return middleware.HTTPError(actErr)
	}
	v, err := h.settings(c, actErr)
	if err != nil {
		return middleware.HTTPError(err)
	}
	status := http.StatusOK
	if actErr != nil {
		status = http.StatusUnprocessableEntity
	} else {
		v.Note = "Saved. A changed limit or protect setting redeploys every running tile."
	}
	return respond.HTML(c, status, comp.SettingsForm(v))
}
