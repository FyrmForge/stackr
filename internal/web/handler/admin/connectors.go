package admin

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/connectors"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/connpanel"
)

// connBase is where the server connectors' actions hang.
const connBase = ui.Base + "/connectors"

// mountConnectors is the Connectors tab's actions. Every one is
// connector.admin, so the route's Require is the whole gate.
func (h *handler) mountConnectors(g *echo.Group, a *middleware.Access) {
	admin := a.Require("connector.admin")
	g.GET(connBase+"/new", h.newConnector, admin)
	// Remove: refused because it is used, the usages, linked, in the dialog
	g.POST(connBase+"/:connector/delete", func(c echo.Context) error {
		err := h.orch.DeleteServerConnector(c.Request().Context(), c.Param("connector"))
		if done, rerr := connpanel.RemoveRefused(c, err); done {
			return rerr
		}
		return h.answer(c, "connectors", "Connector removed.", extra{}, err)
	}, admin)
	h.panel().Mount(g, connBase, admin, nil)
	h.panel().MountDNS(g, dnsBase, admin)
}

// dnsBase is where the server DNS connectors' actions hang.
const dnsBase = ui.Base + "/dns"

// refresh is the tab, whose list a closing dialog swaps in.
func refresh(echo.Context) string { return ui.Href("connectors") }

// panel is the server level of the connectors.
func (h *handler) panel() connpanel.Panel {
	return connpanel.Panel{
		Orch:    h.orch,
		Org:     func(echo.Context) *string { return nil },
		Base:    func(echo.Context) string { return connBase },
		DNSBase: func(echo.Context) string { return dnsBase },
		Delete: func(_ echo.Context, k service.Connector) comp.ConfirmView {
			return comp.ConfirmView{
				Button:  "Remove",
				Title:   "Remove " + k.Name + "?",
				Warning: connpanel.Warning(k),
				Action:  connBase + "/" + k.ID + "/delete",
				Target:  ui.Target,
			}
		},
		Back:    func(echo.Context) string { return connBase + "/new?pick=1" },
		Refresh: refresh,
	}
}

// GET /-/admin/connectors/new: the add dialog on its kind picker. Its Back
// (?pick=1) answers only the picker, into the open dialog.
func (h *handler) newConnector(c echo.Context) error {
	kinds := h.panel().Kinds(c, "")
	if c.QueryParam("pick") != "" {
		return respond.HTML(c, http.StatusOK, connectors.Picker(kinds))
	}
	return respond.HTML(c, http.StatusOK, connectors.Modal("Add a connector", refresh(c), connectors.Picker(kinds)))
}

// connectorsTab is the list: every server connector, each summed up by its
// capabilities.
func (h *handler) connectorsTab(c echo.Context) (templ.Component, error) {
	// no title or help: the section heading says both
	v := connectors.ListView{
		Add:    connBase + "/new",
		Synced: h.panel().Synced(c),
	}
	var err error
	if v.Rows, err = h.panel().Rows(c); err != nil {
		return nil, err
	}
	return connectors.Tab(v, nil), nil
}
