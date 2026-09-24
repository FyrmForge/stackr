package canvas

import (
	"net/http"
	"strconv"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/dialog"
	stackui "github.com/FyrmForge/stackr/internal/ui/drawer/stack"
)

func (h *handler) stackTab(c echo.Context, cd card, f *comp.DrawerView) (templ.Component, error) {
	ctx, st := c.Request().Context(), cd.s.Stack
	switch f.Tab {
	case "params":
		return h.vars(c, cd.s, "", "")
	case "releases":
		rs, err := h.orch.Releases(ctx, st.ID)
		var v stackui.ReleasesView
		for _, r := range rs {
			v.Rows = append(v.Rows, stackui.ReleaseRow{
				Number:  strconv.Itoa(r.Number),
				Created: day(r.CreatedAt),
				By:      r.CreatedBy,
			})
		}
		return stackui.Releases(v), err
	}
	cs, err := h.orch.Connectors(ctx, cd.s.Org.ID)
	v := stackui.SettingsView{
		Name:      st.Name,
		Connector: st.ConfigConnectorID,
		Repo:      st.ConfigRepo,
		Branch:    st.ConfigBranch,
		Path:      st.ConfigPath,
	}
	for _, k := range cs {
		v.Connectors = append(v.Connectors, stackui.Option{Value: k.ID, Label: k.Name + " (" + k.Host + ")"})
	}
	if can(c, cd.s, "stack.write") {
		v.Base = f.Base
		v.Delete = dialog.DeleteStack(st.Name, f.Base+"/delete", "#"+comp.DrawerRoot)
	}
	return stackui.Settings(v), err
}

func (h *handler) stackAction(tab string, do func(echo.Context, card) (string, error)) echo.HandlerFunc {
	return func(c echo.Context) error {
		cd, _ := h.cardOf(c, "stack")
		note, err := do(c, cd)
		if err == nil && c.Response().Committed {
			return nil
		}
		return h.after(c, cd, tab, note, err)
	}
}

// redirect sends the page to url (a moved slug, a deleted card).
func redirect(c echo.Context, url string) (string, error) {
	c.Response().Header().Set("HX-Redirect", url)
	return "", c.NoContent(http.StatusOK)
}

func (h *handler) mountStack(site *echo.Group, a *middleware.Access) {
	s := "/:org/:stack/-/drawer"
	site.GET(s, h.drawerRoute("stack"), a.Require("org.read"))
	write := a.Require("stack.write")
	site.POST(s+"/rename", h.stackAction("settings", func(c echo.Context, cd card) (string, error) {
		st, err := h.orch.RenameStack(c.Request().Context(), cd.s.Stack.ID, c.FormValue("name"))
		if err != nil {
			return "", err
		}
		return redirect(c, "/"+cd.s.Org.Slug+"?drawer=stack:"+st.ID+"&tab=settings")
	}), write)
	site.POST(s+"/config", h.stackAction("settings", func(c echo.Context, cd card) (string, error) {
		f := c.FormValue
		_, err := h.orch.SetConfigRepo(
			c.Request().Context(),
			cd.s.Stack.ID,
			f("connector"),
			f("repo"),
			f("branch"),
			f("path"),
		)
		return "Config repo saved.", err
	}), write)
	site.POST(s+"/delete", h.stackAction("settings", func(c echo.Context, cd card) (string, error) {
		if err := h.orch.DeleteStack(c.Request().Context(), cd.s.Stack.ID); err != nil {
			return "", err
		}
		return redirect(c, "/"+cd.s.Org.Slug)
	}), write)
}
