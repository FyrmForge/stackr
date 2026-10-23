package canvas

import (
	"net/http"
	"slices"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/connectors"
	"github.com/FyrmForge/stackr/internal/web/connpanel"
)

// The org drawer's Connectors tab (owner): the org's connectors of every
// kind and the server ones shared with it, and the add dialog. A git
// connection opens its own drawer (its Repos tab); the others their
// settings dialog.

func orgSlug(c echo.Context) string { return "/" + middleware.ScopeOf(c).Org.Slug }

// connBase is where the tab's add dialog and settings hang.
func connBase(c echo.Context) string { return orgSlug(c) + "/-/drawer/connectors" }

func orgRefresh(c echo.Context) string { return orgSlug(c) + "/-/drawer?tab=connectors" }

// panel is the org level of the connectors.
func (h *handler) panel() connpanel.Panel {
	return connpanel.Panel{
		Orch: h.orch,
		Org: func(c echo.Context) *string {
			id := middleware.ScopeOf(c).Org.ID
			return &id
		},
		Base:    connBase,
		DNSBase: func(c echo.Context) string { return orgSlug(c) + "/-/drawer/dns" },
		Delete: func(c echo.Context, k service.Connector) comp.ConfirmView {
			return comp.ConfirmView{
				Button:  "Remove",
				Title:   "Remove " + k.Name + "?",
				Warning: connpanel.Warning(k),
				Action:  connBase(c) + "/" + k.ID + "/delete",
				Target:  "#" + comp.DrawerRoot,
			}
		},
		Back:    func(c echo.Context) string { return connBase(c) + "/new?pick=1" },
		Refresh: orgRefresh,
		Return: func(c echo.Context) string {
			s := middleware.ScopeOf(c)
			return "/" + s.Org.Slug + "?drawer=org:" + s.Org.ID + "&tab=connectors"
		},
		Drawer: func(c echo.Context, k service.Connector) string { return orgSlug(c) + "/-/connectors/" + k.ID },
	}
}

func (h *handler) mountDNS(site *echo.Group, a *middleware.Access) {
	const conn = "/:org/-/drawer/connectors"
	owner, write := a.Require("connector.manage"), a.Require("connector.write")
	site.POST(conn+"/:connector/delete", h.orgAction("connectors", func(c echo.Context, og *service.Org) (string, error) {
		return "Connector removed.", h.orch.RemoveConnector(c.Request().Context(), &og.ID, c.Param("connector"))
	}), owner)
	// a git kind is a writer's to add, as the org's git connector always was
	h.panel().Mount(site, conn, owner, func(k service.ConnectorKind) echo.MiddlewareFunc {
		if slices.Contains(k.Caps, service.CapGit) {
			return write
		}
		return owner
	})
	h.panel().MountDNS(site, "/:org/-/drawer/dns", a.Require("dns.manage"))
	site.GET(conn+"/new", h.newOrgConnector, owner)
}

// kinds is the add dialog's picker, by what the viewer may add.
func (h *handler) kinds(c echo.Context) []connectors.Kind {
	s := middleware.ScopeOf(c)
	var out []connectors.Kind
	if can(c, s, "connector.write") {
		out = append(out, h.panel().Kinds(c, service.CapGit)...)
	}
	if can(c, s, "connector.manage") {
		for _, k := range h.panel().KindsWith(service.CapDNS) {
			out = append(out, connectors.Kind{Name: k.Label, Hint: k.Hint, URL: connBase(c) + "/new/" + k.Name})
		}
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

// connectorsTab is the list: the org's own connectors (a git one opens its
// drawer, the others their dialog), then the server git connections shared
// with it once connected. add opens the add dialog.
func (h *handler) connectorsTab(c echo.Context, og *service.Org) (templ.Component, error) {
	v := connectors.ListView{
		Title: "Connectors", Scope: "organization",
		Help: "Git hosts and DNS accounts this organization uses.",
		Add:  connBase(c) + "/new",
	}
	rows, err := h.panel().Rows(c)
	if err != nil {
		return nil, err
	}
	v.Rows = rows
	shared, err := h.orch.ConnectedConnectors(c.Request().Context(), og.ID)
	if err != nil {
		return nil, err
	}
	for _, k := range shared {
		if !k.Shared {
			continue
		}
		v.Rows = append(v.Rows, connectors.Row{
			ID: k.ID, Kind: h.kindLabel(k.Kind), Name: k.Name, Summary: "Shared by the server",
			Drawer: orgSlug(c) + "/-/connectors/" + k.ID, Push: "?drawer=connector:" + k.ID,
		})
	}
	var open templ.Component
	if c.QueryParam("add") != "" {
		open = h.addDialog(c)
	}
	return connectors.Tab(v, open), nil
}

// kindLabel is how the screens name kind.
func (h *handler) kindLabel(kind string) string {
	if k, err := h.orch.ConnectorKindOf(kind); err == nil {
		return k.Label
	}
	return kind
}
