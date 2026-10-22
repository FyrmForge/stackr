// Package dnspanel is the DNS connector section's handlers, shared by the
// admin drawer (server connectors) and the org drawer (the org's own). A
// handler mounts them under its own base and gate; Level says whose
// connectors a request reaches.
package dnspanel

import (
	"net/http"
	"slices"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/dns"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// Panel is one level. Org gives the request's org (nil = the server's);
// Base is where the routes below hang; Delete is a row's remove action.
type Panel struct {
	Orch   *service.Orchestrator
	Org    func(c echo.Context) *string
	Base   func(c echo.Context) string
	Delete func(c echo.Context, conn service.Connector) comp.ConfirmView
}

// Mount adds the routes under prefix (the level's base pattern) behind gate.
func (p Panel) Mount(g *echo.Group, prefix string, gate echo.MiddlewareFunc) {
	g.POST(prefix+"/:connector/domains", p.domains, gate)
	g.POST(prefix+"/:connector/manage", p.manage, gate)
	g.POST(prefix+"/:connector/unmanage", p.unmanage, gate)
	g.POST(prefix+"/:connector/takeover", p.takeover, gate)
}

// Form is a refused add, to show again: the user and IP, never the key.
type Form struct {
	User, IP string
	Errors   map[string]string
}

// Add runs the Namecheap form. A refusal comes back as a Form; err is
// only what the page cannot show.
func (p Panel) Add(c echo.Context) (*Form, error) {
	a := service.DNSAccount{User: c.FormValue("user"), Key: c.FormValue("key"), ClientIP: c.FormValue("ip")}
	_, err := p.Orch.AddDNSConnector(c.Request().Context(), p.Org(c), service.DNSNamecheap, a)
	if err == nil {
		return nil, nil
	}
	f := &Form{User: a.User, IP: a.ClientIP, Errors: map[string]string{}}
	if inv, ok := errs.IsInvalid(err); ok && inv.Field != "" {
		f.Errors[inv.Field] = inv.Msg
		return f, nil
	}
	msg, ok := render.Refused(err)
	if !ok {
		return nil, err
	}
	f.Errors["general"] = msg
	return f, nil
}

// View is the section: the form (refused values kept) and the rows.
func (p Panel) View(c echo.Context, f *Form, help string) (ui.View, error) {
	ctx := c.Request().Context()
	org := p.Org(c)
	v := ui.View{Scope: "organization", Help: help, Add: p.Base(c) + "/namecheap", IP: p.Orch.DNSClientIP(ctx)}
	if org == nil {
		v.Scope = "instance"
	}
	if f != nil {
		v.User, v.IP, v.Errors = f.User, f.IP, f.Errors
	}
	cs, err := p.Orch.DNSConnectors(ctx, org)
	if err != nil {
		return v, err
	}
	for _, k := range cs {
		v.Rows = append(v.Rows, ui.Row{ID: k.ID, Name: k.Name, Domains: p.Base(c) + "/" + k.ID + "/domains", Delete: p.Delete(c, k)})
	}
	return v, nil
}

// POST <base>/:connector/domains: the list, asked of the provider.
func (p Panel) domains(c echo.Context) error { return p.answer(c, "", nil) }

// POST <base>/:connector/manage (domain, org...): manage it, or set who
// may use it.
func (p Panel) manage(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	_, err = p.Orch.ManageDNSDomain(c.Request().Context(), p.Org(c), c.Param("connector"), form.Get("domain"), form["org"])
	return p.answer(c, "Saved.", err)
}

func (p Panel) unmanage(c echo.Context) error {
	err := p.Orch.UnmanageDNSDomain(c.Request().Context(), p.Org(c), c.Param("connector"), c.FormValue("domain"))
	return p.answer(c, "No longer managed.", err)
}

func (p Panel) takeover(c echo.Context) error {
	err := p.Orch.TakeOverDNSDomain(c.Request().Context(), p.Org(c), c.Param("connector"), c.FormValue("domain"), c.FormValue("confirm"))
	return p.answer(c, "Switched to Namecheap DNS.", err)
}

// answer is the list again, the action's refusal or note on top. A
// provider that does not answer is the list's text, not an error page.
func (p Panel) answer(c echo.Context, note string, actErr error) error {
	ctx, id := c.Request().Context(), c.Param("connector")
	org := p.Org(c)
	base := p.Base(c) + "/" + id
	v := ui.DomainsView{ID: id, Shared: org == nil, Note: note}
	status := http.StatusOK
	if actErr != nil {
		msg, ok := render.Refused(actErr)
		if !ok {
			return middleware.HTTPError(actErr)
		}
		v.Error, v.Note, status = msg, "", http.StatusUnprocessableEntity
	}
	ds, err := p.Orch.DNSDomains(ctx, org, id)
	if err != nil {
		msg, ok := render.Refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		v.Error = msg
		return respond.HTML(c, status, ui.Domains(v))
	}
	var orgs []service.Org
	if org == nil {
		if orgs, err = p.Orch.AllOrgs(ctx); err != nil {
			return middleware.HTTPError(err)
		}
	}
	for _, d := range ds {
		r := ui.DomainRow{
			Name:     d.Name,
			State:    d.State,
			Manage:   base + "/manage",
			Unmanage: base + "/unmanage",
			TakeOver: base + "/takeover",
			All:      d.ShareAll,
		}
		for _, og := range orgs {
			r.Orgs = append(r.Orgs, ui.Org{ID: og.ID, Name: og.Name, On: !d.ShareAll && slices.Contains(d.OrgIDs, og.ID)})
		}
		v.Rows = append(v.Rows, r)
	}
	return respond.HTML(c, status, ui.Domains(v))
}
