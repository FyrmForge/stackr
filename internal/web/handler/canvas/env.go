package canvas

import (
	"slices"
	"strconv"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/dialog"
	envui "github.com/FyrmForge/stackr/internal/ui/drawer/env"
	"github.com/FyrmForge/stackr/internal/web/render"
)

func (h *handler) envTab(c echo.Context, cd card, f *comp.DrawerView) (templ.Component, error) {
	e, write := cd.s.Env, can(c, cd.s, "env.write")
	switch f.Tab {
	case "params":
		return h.vars(c, cd.s, "", "")
	case "logs":
		return envui.Logs(), nil
	case "releases":
		return h.releases(c, cd, f, write)
	case "order":
		es, err := h.orch.Ladder(c.Request().Context(), e.StackID)
		v := orderView(es)
		if write {
			v.Action = f.Base + "/order"
		}
		return envui.Order(v), err
	}
	v := envui.SettingsView{Name: e.Name, From: e.FromKind, Branch: e.FromBranch, Auto: e.Auto, Color: e.Color}
	if write {
		v.Base = f.Base
		v.Delete = dialog.DeleteEnv(e.Name, f.Base+"/delete", "#"+comp.DrawerRoot)
	}
	return envui.Settings(v), nil
}

// orderView gives each rung the whole order after one move up or down.
func orderView(es []service.Environment) envui.OrderView {
	ids := make([]string, len(es))
	for i, e := range es {
		ids[i] = e.ID
	}
	var v envui.OrderView
	swap := func(i, j int) []string {
		if j < 0 || j >= len(ids) {
			return nil
		}
		o := slices.Clone(ids)
		o[i], o[j] = o[j], o[i]
		return o
	}
	for i, e := range es {
		v.Rungs = append(v.Rungs, envui.Rung{ID: e.ID, Name: e.Name, Color: e.Color, Up: swap(i, i+1), Down: swap(i, i-1)})
	}
	return v
}

// jobKey carries the job an env action queued to the tab that answers it.
const jobKey = "env-drawer-job"

// releases lists the stack's releases; ?plan= adds the dry run of taking
// one into this env and, when nothing blocks it, the confirm that does.
func (h *handler) releases(c echo.Context, cd card, f *comp.DrawerView, write bool) (templ.Component, error) {
	ctx, e := c.Request().Context(), cd.s.Env
	rs, err := h.orch.Releases(ctx, e.StackID)
	if err != nil {
		return nil, err
	}
	var v envui.ReleasesView
	cur, plan := 0, c.QueryParam("plan")
	for _, r := range rs {
		row := envui.ReleaseRow{Number: strconv.Itoa(r.Number), By: r.CreatedBy, Created: day(r.CreatedAt),
			Current: e.ReleaseID != nil && *e.ReleaseID == r.ID, DryRun: f.Base + "?tab=releases&plan=" + r.ID}
		if row.Current {
			cur = r.Number
		}
		v.Rows = append(v.Rows, row)
	}
	if j, ok := c.Get(jobKey).(service.Job); ok {
		jv := render.JobView(urlOf(cd.s), j)
		jv.Refresh = f.Base + "?tab=releases"
		v.Job = &jv
	}
	i := slices.IndexFunc(rs, func(r service.Release) bool { return r.ID == plan })
	if i < 0 {
		return envui.Releases(v), nil
	}
	p, err := h.orch.PlanPromote(ctx, e.ID, plan)
	if err != nil {
		return nil, err
	}
	n, verb := strconv.Itoa(rs[i].Number), "Promote"
	if rs[i].Number < cur {
		verb = "Roll back"
	}
	pv := comp.PlanView{Title: verb + " " + e.Name + " to release #" + n, Blockers: p.Plan.Blockers, Warnings: p.Plan.Warnings, CanDeploy: p.CanDeploy}
	for _, ch := range p.Plan.Changes {
		pv.Changes = append(pv.Changes, comp.ChangeView{Kind: ch.Kind, Tile: ch.Tile, Field: ch.Field, Old: ch.Old, New: ch.New, Note: ch.Note})
	}
	v.Plan = &pv
	if write && p.CanDeploy {
		v.Take = comp.ConfirmView{Button: verb + " to #" + n, Title: pv.Title, Warning: "The plan above is applied; tiles it changes redeploy.",
			Action: f.Base + "/promote/" + plan, Target: "#" + comp.DrawerRoot}
	}
	return envui.Releases(v), nil
}

func (h *handler) envAction(tab string, do func(echo.Context, card) (string, error)) echo.HandlerFunc {
	return func(c echo.Context) error {
		cd, _ := h.cardOf(c, "env")
		note, err := do(c, cd)
		if err == nil && c.Response().Committed {
			return nil
		}
		return h.after(c, cd, tab, note, err)
	}
}

func (h *handler) mountEnv(site *echo.Group, a *middleware.Access) {
	e := "/:org/:stack/:env/-/drawer"
	site.GET(e, h.drawerRoute("env"), a.Require("org.read"))
	write := a.Require("env.write")
	stackURL := func(cd card) string { return "/" + cd.s.Org.Slug + "/" + cd.s.Stack.Slug }
	site.POST(e+"/rename", h.envAction("settings", func(c echo.Context, cd card) (string, error) {
		en, err := h.orch.RenameEnv(c.Request().Context(), cd.s.Env.ID, c.FormValue("name"))
		if err != nil {
			return "", err
		}
		return redirect(c, stackURL(cd)+"?drawer=env:"+en.ID+"&tab=settings")
	}), write)
	site.POST(e+"/from", h.envAction("settings", func(c echo.Context, cd card) (string, error) {
		_, err := h.orch.SetEnvFrom(c.Request().Context(), cd.s.Env.ID, c.FormValue("from"), c.FormValue("branch"), c.FormValue("auto") != "")
		return "Saved.", err
	}), write)
	site.POST(e+"/color", h.envAction("settings", func(c echo.Context, cd card) (string, error) {
		_, err := h.orch.SetEnvColor(c.Request().Context(), cd.s.Env.ID, c.FormValue("color"))
		return "Colour saved.", err
	}), write)
	site.POST(e+"/order", h.envAction("order", func(c echo.Context, cd card) (string, error) {
		form, _ := c.FormParams()
		return "Order saved.", h.orch.ReorderEnvs(c.Request().Context(), cd.s.Stack.ID, form["ids"])
	}), write)
	site.POST(e+"/promote/:release", h.envAction("releases", func(c echo.Context, cd card) (string, error) {
		j, err := h.orch.Promote(c.Request().Context(), cd.s.Env.ID, c.Param("release"))
		if err != nil {
			return "", err
		}
		c.Set(jobKey, j)
		return "Queued: the job below follows it.", nil
	}), write)
	site.POST(e+"/delete", h.envAction("settings", func(c echo.Context, cd card) (string, error) {
		if err := h.orch.DeleteEnv(c.Request().Context(), cd.s.Env.ID); err != nil {
			return "", err
		}
		return redirect(c, stackURL(cd))
	}), write)
}
