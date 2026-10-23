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
	install, err := h.installURL(c, cd)
	if err != nil {
		return nil, err
	}
	if f.Tab == "repos" {
		return connui.Repos(connui.ReposView{Label: h.kindLabel(cd.conn.Kind), Check: f.Base + "/check", InstallURL: install, Shared: cd.conn.Shared}), nil
	}
	v := connui.SettingsView{
		Name:       cd.conn.Name,
		Label:      h.kindLabel(cd.conn.Kind),
		Host:       cd.conn.Host,
		InstallURL: install,
		Shared:     cd.conn.Shared,
	}
	if can(c, cd.s, "connector.write") && !cd.conn.Shared {
		v.Base = f.Base
		v.Delete = dialog.DeleteConnector(cd.conn.Name, h.kindLabel(cd.conn.Kind), f.Base+"/delete", "#"+comp.DrawerRoot)
	}
	return connui.Settings(v), nil
}

// installURL is the provider's install page for the connector, coming back
// to its drawer's Repos tab; "" for a shared server connector, whose
// install is the admin's to manage.
func (h *handler) installURL(c echo.Context, cd card) (string, error) {
	if cd.conn.Shared {
		return "", nil
	}
	back := "/" + cd.s.Org.Slug + "?drawer=connector:" + cd.conn.ID + "&tab=repos"
	return h.orch.ConnectorInstallBack(c.Request().Context(), &cd.s.Org.ID, cd.conn.ID, back)
}

func (h *handler) mountConnector(site *echo.Group, a *middleware.Access) {
	k := "/:org/-/connectors/:connector"
	site.GET(k, h.drawerRoute("connector"), a.Require("org.read"))
	// the install check: one provider round trip, so only when the Repos tab
	// opens or a button asks, never on the drawer's default tab
	site.POST(k+"/check", func(c echo.Context) error {
		cd, err := h.cardOf(c, "connector")
		if err != nil {
			return middleware.HTTPError(err)
		}
		ctx := c.Request().Context()
		install, err := h.installURL(c, cd)
		if err != nil {
			return middleware.HTTPError(err)
		}
		repos, err := h.orch.ConnectorRepos(ctx, cd.s.Org.ID, cd.conn.ID)
		if err != nil {
			return middleware.HTTPError(err)
		}
		v := connui.ReposView{Label: h.kindLabel(cd.conn.Kind), Check: cd.frame("repos").Base + "/check", InstallURL: install, Shared: cd.conn.Shared, Checked: true}
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
