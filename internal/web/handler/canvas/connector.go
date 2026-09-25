package canvas

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/dialog"
	connui "github.com/FyrmForge/stackr/internal/ui/drawer/connector"
)

func (h *handler) connectorTab(c echo.Context, cd card, f *comp.DrawerView) (templ.Component, error) {
	install, err := h.orch.ConnectorInstallURL(c.Request().Context(), cd.s.Org.ID, cd.conn.ID)
	if err != nil {
		return nil, err
	}
	if f.Tab == "repos" {
		return connui.Repos(connui.ReposView{Check: f.Base + "/check", InstallURL: install}), nil
	}
	v := connui.SettingsView{
		Name:       cd.conn.Name,
		Host:       cd.conn.Host,
		InstallURL: install,
	}
	if can(c, cd.s, "connector.write") {
		v.Base = f.Base
		v.Delete = dialog.DeleteConnector(cd.conn.Name, f.Base+"/delete", "#"+comp.DrawerRoot)
	}
	return connui.Settings(v), nil
}

func (h *handler) mountConnector(site *echo.Group, a *middleware.Access) {
	k := "/:org/-/connectors/:connector"
	site.GET(k, h.drawerRoute("connector"), a.Require("org.read"))
	// the install check: one GitHub round trip, so on click, never on a tab open
	site.POST(k+"/check", func(c echo.Context) error {
		cd, err := h.cardOf(c, "connector")
		if err != nil {
			return middleware.HTTPError(err)
		}
		ctx := c.Request().Context()
		install, err := h.orch.ConnectorInstallURL(ctx, cd.s.Org.ID, cd.conn.ID)
		if err != nil {
			return middleware.HTTPError(err)
		}
		repos, err := h.orch.ConnectorRepos(ctx, cd.s.Org.ID, cd.conn.ID)
		if err != nil {
			return middleware.HTTPError(err)
		}
		v := connui.ReposView{Check: cd.frame("repos").Base + "/check", InstallURL: install, Checked: true}
		for _, r := range repos {
			v.Repos = append(v.Repos, r.FullName)
		}
		return respond.HTML(c, http.StatusOK, comp.Drawer(cd.frame("repos"), connui.Repos(v)))
	}, a.Require("org.read"))
	write := a.Require("connector.write")
	site.POST(k+"/rename", func(c echo.Context) error {
		cd, err := h.cardOf(c, "connector")
		if err != nil {
			return middleware.HTTPError(err)
		}
		if cd.conn, err = h.orch.RenameConnector(
			c.Request().Context(),
			cd.s.Org.ID,
			cd.conn.ID,
			c.FormValue("name"),
		); err != nil {
			cd, _ = h.cardOf(c, "connector")
		}
		return h.after(c, cd, "settings", "Renamed.", err)
	}, write)
	site.POST(k+"/delete", func(c echo.Context) error {
		cd, err := h.cardOf(c, "connector")
		if err == nil {
			err = h.orch.DeleteConnector(c.Request().Context(), cd.s.Org.ID, cd.conn.ID)
		}
		if err != nil {
			return h.after(c, cd, "settings", "", err)
		}
		_, err = redirect(c, "/"+cd.s.Org.Slug)
		return err
	}, write)
}
