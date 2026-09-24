package canvas

import (
	"slices"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/dialog"
	envui "github.com/FyrmForge/stackr/internal/ui/drawer/env"
)

func (h *handler) envTab(c echo.Context, cd card, f *comp.DrawerView) (templ.Component, error) {
	e, write := cd.s.Env, can(c, cd.s, "env.write")
	switch f.Tab {
	case "params":
		return h.vars(c, cd.s, "", "")
	case "logs":
		return envui.Logs(), nil
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
	site.POST(e+"/delete", h.envAction("settings", func(c echo.Context, cd card) (string, error) {
		if err := h.orch.DeleteEnv(c.Request().Context(), cd.s.Env.ID); err != nil {
			return "", err
		}
		return redirect(c, stackURL(cd))
	}), write)
}
