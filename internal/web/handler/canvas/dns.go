package canvas

import (
	"net/http"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/connectors"
	"github.com/FyrmForge/stackr/internal/web/dnspanel"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// The org drawer's Connectors tab (owner): the org's GitHub connectors and
// its own DNS connectors, and the add dialog.

func orgSlug(c echo.Context) string { return "/" + middleware.ScopeOf(c).Org.Slug }

// connBase is where the tab's add dialog hangs.
func connBase(c echo.Context) string { return orgSlug(c) + "/-/drawer/connectors" }

func orgRefresh(c echo.Context) string { return orgSlug(c) + "/-/drawer?tab=connectors" }

// dns is the org level of the DNS connectors.
func (h *handler) dns() dnspanel.Panel {
	return dnspanel.Panel{
		Orch: h.orch,
		Org: func(c echo.Context) *string {
			id := middleware.ScopeOf(c).Org.ID
			return &id
		},
		Base: func(c echo.Context) string { return orgSlug(c) + "/-/drawer/dns" },
		Delete: func(c echo.Context, k service.Connector) comp.ConfirmView {
			return comp.ConfirmView{
				Button:  "Remove",
				Title:   "Remove " + k.Name + "?",
				Warning: "stackr deletes the DNS records it made and stops managing its domains. Refused while a tile uses one.",
				Action:  orgSlug(c) + "/-/drawer/dns/" + k.ID + "/delete",
				Target:  "#" + comp.DrawerRoot,
			}
		},
		Back:    func(c echo.Context) string { return connBase(c) + "/new?pick=1" },
		Refresh: orgRefresh,
	}
}

func (h *handler) mountDNS(site *echo.Group, a *middleware.Access) {
	const base = "/:org/-/drawer/dns"
	owner, write := a.Require("dns.manage"), a.Require("connector.write")
	site.POST(base+"/:connector/delete", h.orgAction("connectors", func(c echo.Context, og *service.Org) (string, error) {
		return "Connector removed.", h.orch.DeleteDNSConnector(c.Request().Context(), &og.ID, c.Param("connector"))
	}), owner)
	h.dns().Mount(site, base, owner)
	const conn = "/:org/-/drawer/connectors"
	site.GET(conn+"/new", h.newOrgConnector, owner)
	site.GET(conn+"/new/github", func(c echo.Context) error {
		return respond.HTML(c, http.StatusOK, connectors.GitHub(orgGitHub(c)))
	}, write)
	site.POST(conn+"/github", h.beginOrgConnector, write)
}

// kinds is the add dialog's picker, by what the viewer may add.
func (h *handler) kinds(c echo.Context) []connectors.Kind {
	s := middleware.ScopeOf(c)
	var out []connectors.Kind
	if can(c, s, "connector.write") {
		out = append(out, connectors.Kind{Name: "GitHub", Hint: "A GitHub App this organization owns, for its repositories.", URL: connBase(c) + "/new/github"})
	}
	if can(c, s, "dns.manage") {
		out = append(out, connectors.Kind{Name: "Namecheap", Hint: "DNS records and wildcard certificates for your Namecheap domains.", URL: h.dns().Base(c) + "/namecheap"})
	}
	return out
}

// addDialog is the add dialog on its kind picker.
func (h *handler) addDialog(c echo.Context) templ.Component {
	return connectors.Modal("Add a connector", orgRefresh(c), connectors.Picker(h.kinds(c)))
}

// GET /:org/-/drawer/connectors/new: the add dialog. Its Back (?pick=1)
// answers only the picker, into the open dialog.
func (h *handler) newOrgConnector(c echo.Context) error {
	if c.QueryParam("pick") != "" {
		return respond.HTML(c, http.StatusOK, connectors.Picker(h.kinds(c)))
	}
	return respond.HTML(c, http.StatusOK, h.addDialog(c))
}

func orgGitHub(c echo.Context) connectors.GitHubView {
	return connectors.GitHubView{
		Back:  connBase(c) + "/new?pick=1",
		Again: connBase(c) + "/new/github",
		Begin: connBase(c) + "/github",
	}
}

// POST /:org/-/drawer/connectors/github (github_org): the pending
// connector, and the screen whose form posts its manifest to GitHub.
func (h *handler) beginOrgConnector(c echo.Context) error {
	v := orgGitHub(c)
	v.Org = strings.TrimSpace(c.FormValue("github_org"))
	_, action, manifest, err := h.orch.BeginConnector(c.Request().Context(), middleware.ScopeOf(c).Org.ID, middleware.Principal(c).User.ID, v.Org)
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

// connectorsTab is the list: GitHub connectors (each opens its own
// drawer), then the org's DNS connectors. add opens the add dialog.
func (h *handler) connectorsTab(c echo.Context, og *service.Org) (templ.Component, error) {
	cs, err := h.orch.Connectors(c.Request().Context(), og.ID)
	if err != nil {
		return nil, err
	}
	done, err := h.orch.ConnectedConnectors(c.Request().Context(), og.ID)
	if err != nil {
		return nil, err
	}
	connected := map[string]bool{}
	for _, k := range done {
		connected[k.ID] = true
	}
	v := connectors.ListView{
		Title: "Connectors", Scope: "organization",
		Help: "GitHub Apps and DNS accounts this organization uses.",
		Add:  connBase(c) + "/new",
	}
	for _, k := range cs {
		// a server App the admin has not finished is the admin's, not here
		if k.Shared && !connected[k.ID] {
			continue
		}
		r := connectors.Row{ID: k.ID, Kind: "GitHub", Name: k.Name, Pending: !connected[k.ID], Drawer: orgSlug(c) + "/-/connectors/" + k.ID, Push: "?drawer=connector:" + k.ID}
		if k.Shared {
			r.Summary = "Shared by the server"
		}
		v.Rows = append(v.Rows, r)
	}
	dns, err := h.dns().Rows(c)
	if err != nil {
		return nil, err
	}
	v.Rows = append(v.Rows, dns...)
	var open templ.Component
	if c.QueryParam("add") != "" {
		open = h.addDialog(c)
	}
	return connectors.Tab(v, open), nil
}
