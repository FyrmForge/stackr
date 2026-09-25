package canvas

import (
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/dialog"
	connui "github.com/FyrmForge/stackr/internal/ui/drawer/connector"
)

func (h *handler) connectorTab(c echo.Context, cd card, f *comp.DrawerView) (templ.Component, error) {
	if f.Tab == "repos" {
		return connui.Repos(), nil
	}
	v := connui.SettingsView{Name: cd.conn.Name, Host: cd.conn.Host}
	if can(c, cd.s, "connector.write") {
		v.Base = f.Base
		v.Delete = dialog.DeleteConnector(cd.conn.Name, f.Base+"/delete", "#"+comp.DrawerRoot)
	}
	return connui.Settings(v), nil
}

func (h *handler) mountConnector(site *echo.Group, a *middleware.Access) {
	k := "/:org/-/connectors/:connector"
	site.GET(k, h.drawerRoute("connector"), a.Require("org.read"))
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
