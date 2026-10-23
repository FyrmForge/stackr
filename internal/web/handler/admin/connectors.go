package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/connectors"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/dnspanel"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// connBase is where the server connectors' actions hang.
const connBase = ui.Base + "/connectors"

// mountConnectors is the Connectors tab's actions. Every one is
// connector.admin, so the route's Require is the whole gate.
func (h *handler) mountConnectors(g *echo.Group, a *middleware.Access) {
	b, admin := connBase, a.Require("connector.admin")
	g.GET(b+"/new", h.newConnector, admin)
	g.GET(b+"/new/github", h.githubForm, admin)
	g.POST(b, h.beginConnector, admin)
	g.GET(b+"/:connector/settings", h.connectorSettings, admin)
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
	g.POST(b+"/:connector/check", h.checkConnector, admin)
	g.POST(b+"/:connector/delete", h.act("connectors", func(c echo.Context) (string, *service.Job, error) {
		return "Connector removed.", nil, h.orch.DeleteServerConnector(c.Request().Context(), c.Param("connector"))
	}), admin)
	h.dns().Mount(g, dnsBase, admin)
}

// dnsBase is where the server DNS connectors' actions hang.
const dnsBase = ui.Base + "/dns"

// refresh is the tab, whose list a closing dialog swaps in.
func refresh(echo.Context) string { return ui.Base + "?tab=connectors" }

// dns is the server level of the DNS connectors.
func (h *handler) dns() dnspanel.Panel {
	return dnspanel.Panel{
		Orch: h.orch,
		Org:  func(echo.Context) *string { return nil },
		Base: func(echo.Context) string { return dnsBase },
		Delete: func(_ echo.Context, k service.Connector) comp.ConfirmView {
			return comp.ConfirmView{
				Button:  "Remove",
				Title:   "Remove " + k.Name + "?",
				Warning: "stackr deletes the DNS records it made and stops managing its domains. Refused while a tile uses one.",
				Action:  connBase + "/" + k.ID + "/delete",
				Target:  "#" + comp.DrawerRoot,
			}
		},
		Back:    func(echo.Context) string { return connBase + "/new?pick=1" },
		Refresh: refresh,
	}
}

// kinds is the add dialog's picker.
var kinds = []connectors.Kind{
	{Name: "GitHub", Hint: "A GitHub App the server owns, shared with the organizations you pick.", URL: connBase + "/new/github"},
	{Name: "Namecheap", Hint: "DNS records and wildcard certificates for your Namecheap domains.", URL: dnsBase + "/namecheap"},
}

// GET /-/admin/connectors/new: the add dialog on its kind picker. Its Back
// (?pick=1) answers only the picker, into the open dialog.
func (h *handler) newConnector(c echo.Context) error {
	if c.QueryParam("pick") != "" {
		return respond.HTML(c, http.StatusOK, connectors.Picker(kinds))
	}
	return respond.HTML(c, http.StatusOK, connectors.Modal("Add a connector", refresh(c), connectors.Picker(kinds)))
}

func githubView() connectors.GitHubView {
	return connectors.GitHubView{
		Back:  connBase + "/new?pick=1",
		Again: connBase + "/new/github",
		Begin: connBase,
		Help:  "The server owns the app. Share it with organizations once it is made.",
	}
}

// GET /-/admin/connectors/new/github: GitHub's first screen.
func (h *handler) githubForm(c echo.Context) error {
	return respond.HTML(c, http.StatusOK, connectors.GitHub(githubView()))
}

// POST /-/admin/connectors (github_org): the pending server connector,
// answered with the screen whose form posts its manifest to GitHub. Only
// the admin who began can complete it (the callback checks).
func (h *handler) beginConnector(c echo.Context) error {
	v := githubView()
	v.Org = strings.TrimSpace(c.FormValue("github_org"))
	_, action, manifest, err := h.orch.BeginServerConnector(c.Request().Context(), middleware.Principal(c).User.ID, v.Org)
	if err != nil {
		msg, ok := render.Refusal(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		v.Error = msg
		return respond.HTML(c, http.StatusUnprocessableEntity, connectors.GitHub(v))
	}
	v.Action, v.Manifest = action, manifest
	return respond.HTML(c, http.StatusOK, connectors.GitHub(v))
}

// POST /-/admin/connectors/:connector/check: a row's install status, asked
// of GitHub as the row loads. GitHub not answering is a chip, not an error
// page: the rest of the tab still works.
func (h *handler) checkConnector(c echo.Context) error {
	repos, err := h.orch.ServerConnectorRepos(c.Request().Context(), c.Param("connector"))
	if errors.Is(err, errs.ErrNotFound) {
		return middleware.HTTPError(err)
	}
	if err != nil {
		slog.Warn("github install check", "connector", c.Param("connector"), "err", err)
	}
	return respond.HTML(c, http.StatusOK, ui.InstallStatus(len(repos), err != nil))
}

// GET /-/admin/connectors/:connector/settings: a server GitHub connector's
// dialog: name, sharing, Remove.
func (h *handler) connectorSettings(c echo.Context) error {
	ctx := c.Request().Context()
	cs, err := h.orch.ServerConnectors(ctx)
	if err != nil {
		return middleware.HTTPError(err)
	}
	i := slices.IndexFunc(cs, func(k service.ServerConnector) bool { return k.ID == c.Param("connector") })
	if i < 0 {
		return middleware.HTTPError(errs.ErrNotFound)
	}
	k := cs[i]
	install, err := h.orch.ServerConnectorInstallURL(ctx, k.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	ogs, err := h.orch.AllOrgs(ctx)
	if err != nil {
		return middleware.HTTPError(err)
	}
	base := connBase + "/" + k.ID
	v := ui.ServerConnector{
		ID: k.ID, Name: k.Name, InstallURL: install, All: k.ShareAll,
		Rename: base + "/name", Share: base + "/share",
		Delete: comp.ConfirmView{
			Button:  "Remove",
			Title:   "Remove " + k.Name + "?",
			Warning: "Organizations and stacks bound to it must be unbound first. The app stays on GitHub.",
			Action:  base + "/delete",
			Target:  "#" + comp.DrawerRoot,
		},
	}
	for _, og := range ogs {
		v.Orgs = append(v.Orgs, ui.OrgShare{ID: og.ID, Name: og.Name, On: slices.Contains(k.OrgIDs, og.ID)})
	}
	return respond.HTML(c, http.StatusOK, connectors.Modal(k.Name, refresh(c), ui.ConnectorSettings(v)))
}

// connectorsTab is the list: GitHub connectors, then DNS ones.
func (h *handler) connectorsTab(c echo.Context) (templ.Component, error) {
	ctx := c.Request().Context()
	cs, err := h.orch.ServerConnectors(ctx)
	if err != nil {
		return nil, err
	}
	v := connectors.ListView{
		Title: "Connectors", Scope: "instance",
		Help:   "GitHub Apps and DNS accounts the server owns.",
		Add:    connBase + "/new",
		Synced: h.dns().Synced(c),
	}
	for _, k := range cs {
		install, err := h.orch.ServerConnectorInstallURL(ctx, k.ID)
		if err != nil {
			return nil, err
		}
		r := connectors.Row{
			ID: k.ID, Kind: "GitHub", Name: k.Name, Summary: shared(k),
			Settings: connBase + "/" + k.ID + "/settings",
		}
		if install == "" {
			r.Pending = true
		} else {
			r.Check = connBase + "/" + k.ID + "/check"
		}
		v.Rows = append(v.Rows, r)
	}
	dns, err := h.dns().Rows(c)
	if err != nil {
		return nil, err
	}
	v.Rows = append(v.Rows, dns...)
	return connectors.Tab(v, nil), nil
}

// shared is a server GitHub connector's sharing in a few words.
func shared(k service.ServerConnector) string {
	switch {
	case k.ShareAll:
		return "Shared with all organizations"
	case len(k.OrgIDs) == 1:
		return "Shared with 1 organization"
	case len(k.OrgIDs) > 1:
		return "Shared with " + strconv.Itoa(len(k.OrgIDs)) + " organizations"
	}
	return "Not shared yet"
}
