package canvas

import (
	"cmp"
	"context"
	"slices"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/dialog"
	envui "github.com/FyrmForge/stackr/internal/ui/drawer/env"
	"github.com/FyrmForge/stackr/internal/ui/drawer/vars"
	placementui "github.com/FyrmForge/stackr/internal/ui/placement"
	"github.com/FyrmForge/stackr/internal/web/render"
)

func (h *handler) envTab(c echo.Context, cd card, f *comp.DrawerView) (templ.Component, error) {
	ctx, e, write := c.Request().Context(), cd.s.Env, can(c, cd.s, "env.write")
	// a move that ran or runs shows its progress as the last tab
	if ms, err := h.orch.EnvMoveStatus(ctx, e.ID); err == nil && ms != nil && (ms.Active || f.Tab == "moving") {
		f.Tabs = append(slices.Clone(f.Tabs), "moving")
	}
	// the Logs tab needs the kept history, and the verb that reads it
	if !h.orch.LogsOn() || !can(c, cd.s, "tile.read") {
		f.Tabs = slices.DeleteFunc(slices.Clone(f.Tabs), func(t string) bool { return t == "logs" })
		if f.Tab == "logs" {
			f.Tab = f.Tabs[0]
		}
	}
	switch f.Tab {
	case "moving":
		return placementui.Load(urlOf(cd.s) + "/-/placement/move/status"), nil
	case "logs":
		v, err := render.LogHistory(c, h.orch, cd.s)
		if err != nil {
			return nil, err
		}
		if can(c, cd.s, "env.logs.purge") {
			p := dialog.PurgeLogs(e.Name, f.Base+"/logs/purge", "#"+comp.DrawerRoot)
			v.Purge = &p
		}
		return envui.Logs(v), nil
	case "releases":
		return h.releasesTab(c, cd, f)
	case "sync":
		return h.syncTab(c, cd, f)
	}
	return h.envSettings(c, cd, f, write)
}

// envSettings is the Settings tab: one accordion row per group, each closed
// row summing up its values. ?open= (or a saved form's open) names the row
// open on load.
func (h *handler) envSettings(c echo.Context, cd card, f *comp.DrawerView, write bool) (templ.Component, error) {
	ctx, e := c.Request().Context(), cd.s.Env
	ts, err := h.orch.Tiles(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	cascade, err := h.cascadeForm(c, cd, e.Settings)
	if err != nil {
		return nil, err
	}
	changed := anyChanged(cascade.Rows)
	es, err := h.orch.Ladder(ctx, e.StackID)
	if err != nil {
		return nil, err
	}
	varsBody, err := h.vars(c, cd.s, "", "")
	if err != nil {
		return nil, err
	}
	set, secret, err := h.varCounts(c, cd.s)
	if err != nil {
		return nil, err
	}
	pv, err := h.orch.Placement(ctx, service.ScopeEnv, e.ID)
	if err != nil {
		return nil, err
	}
	v := envui.SettingsView{
		Name:      e.Name,
		Tiles:     len(ts),
		Canvas:    urlOf(cd.s),
		Path:      cd.s.Org.Slug + " / " + cd.s.Stack.Slug + " / " + e.Slug,
		From:      e.FromKind,
		Branch:    e.FromBranch,
		Auto:      e.Auto,
		Below:     below(es, e.ID),
		Color:     e.Color,
		Cascade:   cascade,
		Vars:      varsBody,
		Placement: placementui.Load(urlOf(cd.s) + "/-/placement"),
		Open:      c.FormValue("open"),
	}
	if v.Lock, err = h.lockView(c, cd, f.Base); err != nil {
		return nil, err
	}
	v.Sums = envui.Sums{
		General:          envui.GeneralSummary(e.Name, e.Color, v.Lock),
		Deploys:          envui.DeploysSummary(e.FromKind, e.FromBranch, e.Auto, v.Below),
		Resources:        envui.ResourcesSummary(cascade.Rows),
		ResourcesChanged: changed,
		Variables:        envui.VariablesSummary(set, secret),
		Placement:        envui.PlacementSummary(pv.PinName, pv.IngressName, exitOf(pv.Rungs)),
	}
	if write {
		v.Base = f.Base
		v.Delete = dialog.DeleteEnv(e.Name, f.Base+"/delete", "#"+comp.DrawerRoot)
	} else {
		v.Cascade.ReadOnly, v.Cascade.Why = true, "Changing resources needs write access to this environment."
	}
	return envui.Settings(v), nil
}

// anyChanged is true when this rung sets any knob; cascadeRows marks them.
func anyChanged(rows []comp.SettingRowView) bool {
	for _, r := range rows {
		if r.Changed {
			return true
		}
	}
	return false
}

// below is the name of the rung under id on the ladder (bottom first); ""
// for the bottom rung.
func below(es []service.Environment, id string) string {
	for i, e := range es {
		if e.ID == id && i > 0 {
			return es[i-1].Name
		}
	}
	return ""
}

// exitOf is the first exit of the ladder; "" = the env's own uplink.
func exitOf(rs []service.PlacementRung) string {
	if len(rs) == 0 || rs[0].Own {
		return ""
	}
	return rs[0].Name
}

// varCounts is how many variables this env's own grid sets, and how many
// of them are secret.
// ponytail: reads the grid a second time beside h.vars; share it if the
// Settings tab gets slow.
func (h *handler) varCounts(c echo.Context, s service.Scope) (set, secret int, err error) {
	ctx := c.Request().Context()
	cs, _, err := h.cols(ctx, s)
	if err != nil {
		return 0, 0, err
	}
	g, err := h.grid(ctx, cs, can(c, s, "variable.write"))
	if err != nil {
		return 0, 0, err
	}
	for _, gr := range g.Groups {
		for _, r := range gr.Rows {
			if !slices.ContainsFunc(r.Cells, func(c vars.Cell) bool { return c.Set }) {
				continue
			}
			set++
			if r.Secret {
				secret++
			}
		}
	}
	return set, secret, nil
}

// lockView: a PR env has no lock; a tiered env shows its tier's.
func (h *handler) lockView(c echo.Context, cd card, base string) (envui.LockView, error) {
	e := cd.s.Env
	v := envui.LockView{Show: e.Type == "static", Locked: e.Locked, Name: e.Slug, Tiers: "/" + cd.s.Org.Slug + "?drawer=org:" + cd.s.Org.ID}
	if !v.Show {
		return v, nil
	}
	if can(c, cd.s, "env.lock") {
		v.Base = base + "/lock"
	}
	ts, err := h.orch.Tiers(c.Request().Context(), cd.s.Org.ID)
	for _, t := range ts {
		if t.Slug == e.Slug {
			v.Tiered, v.Locked = true, t.Locked
		}
	}
	return v, err
}

// orderView gives each rung the whole order after one move up or down.
// The env drawer's Order tab is gone; the stack drawer's Env order row
// takes this and POST /order.
//
//nolint:unused // kept for the stack drawer's Env order row
func orderView(es []service.Environment) envui.OrderView {
	ids := make([]string, len(es))
	for i, e := range es {
		ids[i] = e.ID
	}
	hues := service.EnvHues(es)
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
		v.Rungs = append(v.Rungs, envui.Rung{
			ID:    e.ID,
			Name:  e.Name,
			Color: hues[e.ID],
			Up:    swap(i, i+1),
			Down:  swap(i, i-1),
		})
	}
	return v
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
		ctx := c.Request().Context()
		err := h.orch.JoinsLockedTier(ctx, middleware.Principal(c), cd.s.Stack.ID, c.FormValue("name"), cd.s.Env.Slug)
		if err != nil {
			return "", err
		}
		en, err := h.orch.RenameEnv(ctx, cd.s.Env.ID, c.FormValue("name"))
		if err != nil {
			return "", err
		}
		return redirect(c, stackURL(cd)+"?drawer=env:"+en.ID+"&tab=settings&open=general")
	}), write)
	site.POST(e+"/from", h.envAction("settings", func(c echo.Context, cd card) (string, error) {
		_, err := h.orch.SetEnvFrom(
			c.Request().Context(),
			cd.s.Env.ID,
			c.FormValue("from"),
			c.FormValue("branch"),
			c.FormValue("auto") != "",
		)
		return "Saved.", err
	}), write)
	site.POST(e+"/color", h.envAction("settings", func(c echo.Context, cd card) (string, error) {
		_, err := h.orch.SetEnvColor(c.Request().Context(), cd.s.Env.ID, c.FormValue("color"))
		return "Colour saved.", err
	}), write)
	site.POST(e+"/lock", func(c echo.Context) error {
		cd, _ := h.cardOf(c, "env")
		_, err := h.orch.SetEnvLock(c.Request().Context(), cd.s.Env.ID, c.FormValue("locked") != "")
		return h.after(c, cd, cmp.Or(c.FormValue("tab"), "settings"), "Lock saved.", err)
	}, a.Require("env.lock"))
	site.POST(e+"/order", h.envAction("settings", func(c echo.Context, cd card) (string, error) {
		form, _ := c.FormParams()
		return "Order saved.", h.orch.ReorderEnvs(c.Request().Context(), cd.s.Stack.ID, form["ids"])
	}), write)
	site.POST(e+"/promote/:release", h.envAction("releases", func(c echo.Context, cd card) (string, error) {
		j, err := h.orch.PromoteTicked(c.Request().Context(), middleware.Principal(c), cd.s.Env.ID, c.Param("release"), render.ApproveOpts(c).Ticked)
		if err != nil {
			return "", err
		}
		c.Set(jobKey, queued{job: j, page: urlOf(cd.s)})
		return "Queued: the job below follows it.", nil
	}), write)
	site.POST(e+"/sync/:from", h.envAction("sync", func(c echo.Context, cd card) (string, error) {
		form, _ := c.FormParams()
		j, err := h.orch.EnvSync(c.Request().Context(), cd.s.Env.ID, c.Param("from"), form["keep"], c.FormValue("sig"))
		if msg, ok := refused(err); err != nil && ok && c.FormValue("bar") != "" {
			// the bar has no drawer to show the refusal in: back to the
			// review, re-planned, with the reason as a flash
			hamrmw.SetFlash(c, msg, hamrmw.FlashError)
			return redirect(c, urlOf(cd.s)+"?drawer=env:"+cd.s.Env.ID+"&tab=sync&"+syncQ{from: c.Param("from"), drop: dropList(c.FormValue("drop"))}.query())
		}
		if err != nil {
			return "", err
		}
		// the full page leaves the review (a ?job= page draws none: no tags,
		// ghosts or bar, the stream back); sync= only names the drawer's source
		return redirect(c, urlOf(cd.s)+"?drawer=env:"+cd.s.Env.ID+"&tab=sync&sync="+c.Param("from")+"&job="+j.ID)
	}), write)
	site.POST(e+"/settings", h.saveRung(
		"env",
		func(s service.Scope) string { return s.Env.Settings },
		func(ctx context.Context, s service.Scope, blob string) error {
			_, err := h.orch.SetEnvSettings(ctx, s.Env.ID, blob)
			return err
		},
	), write)
	site.POST(e+"/logs/purge", h.envAction("logs", func(c echo.Context, cd card) (string, error) {
		return "Logs deleted.", h.orch.PurgeLogs(c.Request().Context(), cd.s)
	}), a.Require("env.logs.purge"))
	site.POST(e+"/delete", h.envAction("settings", func(c echo.Context, cd card) (string, error) {
		if err := h.orch.DeleteEnv(c.Request().Context(), cd.s.Env.ID); err != nil {
			return "", err
		}
		return redirect(c, stackURL(cd))
	}), write)
}
