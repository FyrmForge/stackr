package canvas

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	dnsui "github.com/FyrmForge/stackr/internal/ui/dns"
	"github.com/FyrmForge/stackr/internal/web/dnspanel"
)

// The org drawer's Connectors tab: the org's own DNS connectors (owner).

// dns is the org level of the DNS connector section.
func (h *handler) dns() dnspanel.Panel {
	return dnspanel.Panel{
		Orch: h.orch,
		Org: func(c echo.Context) *string {
			id := middleware.ScopeOf(c).Org.ID
			return &id
		},
		Base: func(c echo.Context) string { return "/" + middleware.ScopeOf(c).Org.Slug + "/-/drawer/dns" },
		Delete: func(c echo.Context, k service.Connector) comp.ConfirmView {
			return comp.ConfirmView{
				Button:  "Remove",
				Title:   "Remove " + k.Name + "?",
				Warning: "stackr stops managing its domains. Their records stay as they are.",
				Action:  "/" + middleware.ScopeOf(c).Org.Slug + "/-/drawer/dns/" + k.ID + "/delete",
				Target:  "#" + comp.DrawerRoot,
			}
		},
	}
}

func (h *handler) mountDNS(site *echo.Group, a *middleware.Access) {
	const base = "/:org/-/drawer/dns"
	owner := a.Require("dns.manage")
	site.POST(base+"/namecheap", h.addNamecheap, owner)
	site.POST(base+"/:connector/delete", h.orgAction("connectors", func(c echo.Context, og *service.Org) (string, error) {
		return "Connector removed.", h.orch.DeleteDNSConnector(c.Request().Context(), &og.ID, c.Param("connector"))
	}), owner)
	h.dns().Mount(site, base, owner)
}

// POST /:org/-/drawer/dns/namecheap: check the account and save it; a
// refusal shows the form again, the key blank.
func (h *handler) addNamecheap(c echo.Context) error {
	cd, err := h.cardOf(c, "org")
	if err != nil {
		return middleware.HTTPError(err)
	}
	form, err := h.dns().Add(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	f := cd.frame("connectors")
	status := http.StatusOK
	if form != nil {
		status = http.StatusUnprocessableEntity
	} else {
		f.Note = "Connected. Pick the domains stackr manages below."
	}
	v, err := h.dns().View(c, form, orgDNSHelp)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, status, comp.Drawer(f, dnsui.Tab(v)))
}

const orgDNSHelp = "This organization's own accounts. Their domains are this organization's only."
