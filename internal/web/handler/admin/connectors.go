package admin

import (
	"net/http"
	"slices"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// begun is the manifest a started server connector hands the browser to
// post to GitHub.
type begun struct{ action, manifest string }

// mountConnectors is the Connectors tab's actions. Every one is
// connector.admin, so the route's Require is the whole gate.
func (h *handler) mountConnectors(g *echo.Group, a *middleware.Access) {
	b, admin := ui.Base+"/connectors", a.Require("connector.admin")
	g.POST(b, h.beginConnector, admin)
	g.POST(b+"/:connector/name", h.act("connectors", func(c echo.Context) (string, *service.Job, error) {
		_, err := h.orch.RenameServerConnector(c.Request().Context(), c.Param("connector"), c.FormValue("name"))
		return "Renamed.", nil, err
	}), admin)
	g.POST(b+"/:connector/share", h.act("connectors", func(c echo.Context) (string, *service.Job, error) {
		form, err := c.FormParams()
		if err != nil {
			return "", nil, echo.NewHTTPError(http.StatusBadRequest, "bad form")
		}
		_, err = h.orch.ShareConnector(c.Request().Context(), c.Param("connector"), form["org"], form.Get("all") != "")
		return "Saved.", nil, err
	}), admin)
	g.POST(b+"/:connector/delete", h.act("connectors", func(c echo.Context) (string, *service.Job, error) {
		return "Connector removed.", nil, h.orch.DeleteServerConnector(c.Request().Context(), c.Param("connector"))
	}), admin)
}

// POST /-/admin/connectors: the pending server connector, answered with the
// tab carrying the form that posts its manifest to GitHub. Only the admin
// who began can complete it (the callback checks).
func (h *handler) beginConnector(c echo.Context) error {
	ctx := c.Request().Context()
	f, status := frame("connectors"), http.StatusOK
	var b *begun
	_, action, manifest, err := h.orch.BeginServerConnector(
		ctx,
		middleware.Principal(c).User.ID,
		strings.TrimSpace(c.FormValue("github_org")),
	)
	if err != nil {
		msg, ok := render.Refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		f.Error, status = msg, http.StatusUnprocessableEntity
	} else {
		b = &begun{action, manifest}
	}
	body, err := h.connectorsTab(c, b)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, status, comp.Drawer(f, body))
}

func (h *handler) connectorsTab(c echo.Context, b *begun) (templ.Component, error) {
	ctx := c.Request().Context()
	cs, err := h.orch.ServerConnectors(ctx)
	if err != nil {
		return nil, err
	}
	ogs, err := h.orch.AllOrgs(ctx)
	if err != nil {
		return nil, err
	}
	v := ui.ConnectorsView{Begin: ui.Base + "/connectors"}
	if b != nil {
		v.Action, v.Manifest = b.action, b.manifest
	}
	for _, k := range cs {
		install, err := h.orch.ServerConnectorInstallURL(ctx, k.ID)
		if err != nil {
			return nil, err
		}
		base := ui.Base + "/connectors/" + k.ID
		r := ui.ServerConnector{
			ID:         k.ID,
			Name:       k.Name,
			Host:       k.Host,
			InstallURL: install,
			All:        k.ShareAll,
			Rename:     base + "/name",
			Share:      base + "/share",
			Delete: comp.ConfirmView{
				Button:  "Remove",
				Title:   "Remove " + k.Name + "?",
				Warning: "Organizations and stacks bound to it must be unbound first. The app stays on GitHub.",
				Action:  base + "/delete",
				Target:  "#" + comp.DrawerRoot,
			},
		}
		for _, og := range ogs {
			r.Orgs = append(r.Orgs, ui.OrgShare{ID: og.ID, Name: og.Name, On: slices.Contains(k.OrgIDs, og.ID)})
		}
		v.Rows = append(v.Rows, r)
	}
	return ui.Connectors(v), nil
}
