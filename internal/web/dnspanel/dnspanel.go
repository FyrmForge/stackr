// Package dnspanel is the DNS connector handlers, shared by the admin
// drawer (server connectors), the org drawer (the org's own) and the setup
// wizard's Certificates step. A handler mounts them under its own base and
// gate; Org says whose connectors a request reaches.
package dnspanel

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/connectors"
	ui "github.com/FyrmForge/stackr/internal/ui/dns"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// Panel is one level. Org gives the request's org (nil = the server's);
// Base is where the routes below hang; Delete is a connector's remove
// action (Action "" = none); Back is the add dialog's kind picker (nil =
// the dialog opens straight on Namecheap); Refresh is a GET whose answer
// holds the connectors list, fetched as a dialog closes.
type Panel struct {
	Orch    *service.Orchestrator
	Org     func(c echo.Context) *string
	Base    func(c echo.Context) string
	Delete  func(c echo.Context, conn service.Connector) comp.ConfirmView
	Back    func(c echo.Context) string
	Refresh func(c echo.Context) string
}

// Mount adds the routes under prefix (the level's base pattern) behind gate.
func (p Panel) Mount(g *echo.Group, prefix string, gate echo.MiddlewareFunc) {
	g.GET(prefix+"/namecheap", p.form, gate)
	g.POST(prefix+"/namecheap", p.add, gate)
	g.GET(prefix+"/:connector/settings", p.settings, gate)
	g.POST(prefix+"/:connector/domains", p.domains, gate)
	g.POST(prefix+"/:connector/manage", p.manage, gate)
	g.POST(prefix+"/:connector/unmanage", p.unmanage, gate)
	g.POST(prefix+"/:connector/takeover", p.takeover, gate)
}

// Form is Namecheap's first screen in the add dialog.
func (p Panel) Form(c echo.Context) ui.AddView {
	v := ui.AddView{Add: p.Base(c) + "/namecheap", IP: p.Orch.DNSClientIP(c.Request().Context())}
	if p.Back != nil {
		v.Back = p.Back(c)
	}
	return v
}

// Open is the add dialog opened straight on Namecheap (the wizard's).
func (p Panel) Open(c echo.Context) templ.Component {
	return connectors.Modal("Add a Namecheap connector", p.Refresh(c), ui.Add(p.Form(c)))
}

// GET <base>/namecheap: Namecheap's screens, into the add dialog.
func (p Panel) form(c echo.Context) error {
	return respond.HTML(c, http.StatusOK, ui.Add(p.Form(c)))
}

// POST <base>/namecheap (user, key, ip): check the account and save it,
// then pick its domains. A refusal shows the screens again on the step
// with the first error, the user and IP kept, never the key.
func (p Panel) add(c echo.Context) error {
	a := service.DNSAccount{User: c.FormValue("user"), Key: c.FormValue("key"), ClientIP: c.FormValue("ip")}
	k, err := p.Orch.AddDNSConnector(c.Request().Context(), p.Org(c), service.DNSNamecheap, a)
	if err == nil {
		v := ui.AddView{ID: k.Connector.ID, Domains: p.Base(c) + "/" + k.Connector.ID + "/domains"}
		return respond.HTML(c, http.StatusOK, ui.Add(v))
	}
	v := p.Form(c)
	v.User, v.IP, v.Errors = a.User, a.ClientIP, fields(err)
	if len(v.Errors) == 0 {
		msg, ok := render.Refusal(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		v.Errors["general"] = msg
	}
	return respond.HTML(c, http.StatusUnprocessableEntity, ui.Add(v))
}

// fields is every field error in err, which may join several.
func fields(err error) map[string]string {
	out := map[string]string{}
	var walk func(error)
	walk = func(e error) {
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, x := range j.Unwrap() {
				walk(x)
			}
			return
		}
		if inv, ok := errs.IsInvalid(e); ok && inv.Field != "" {
			out[inv.Field] = inv.Msg
		}
	}
	walk(err)
	return out
}

// connector is the request's connector of this level.
func (p Panel) connector(c echo.Context) (service.Connector, error) {
	cs, err := p.Orch.DNSConnectors(c.Request().Context(), p.Org(c))
	if err != nil {
		return service.Connector{}, err
	}
	i := slices.IndexFunc(cs, func(k service.Connector) bool { return k.ID == c.Param("connector") })
	if i < 0 {
		return service.Connector{}, errs.ErrNotFound
	}
	return cs[i], nil
}

// GET <base>/:connector/settings: the settings dialog, its domains and
// Remove.
func (p Panel) settings(c echo.Context) error {
	k, err := p.connector(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	v := ui.SettingsView{ID: k.ID, Domains: p.Base(c) + "/" + k.ID + "/domains", Delete: p.Delete(c, k)}
	return respond.HTML(c, http.StatusOK, connectors.Modal(k.Name, p.Refresh(c), ui.Settings(v)))
}

// Rows is the level's DNS connectors in the list.
func (p Panel) Rows(c echo.Context) ([]connectors.Row, error) {
	ctx, org := c.Request().Context(), p.Org(c)
	cs, err := p.Orch.DNSConnectors(ctx, org)
	if err != nil {
		return nil, err
	}
	n, err := p.Orch.DNSManaged(ctx, org)
	if err != nil {
		return nil, err
	}
	var out []connectors.Row
	for _, k := range cs {
		out = append(out, connectors.Row{
			ID: k.ID, Kind: "DNS", Name: k.Name, Summary: managed(n[k.ID]),
			Settings: p.Base(c) + "/" + k.ID + "/settings",
		})
	}
	return out, nil
}

func managed(n int) string {
	switch n {
	case 0:
		return "No managed domains"
	case 1:
		return "1 managed domain"
	}
	return fmt.Sprintf("%d managed domains", n)
}

// Synced is the last records sync's failure; it can name any org's
// hosts, so only the server level shows it.
func (p Panel) Synced(c echo.Context) string {
	if p.Org(c) != nil {
		return ""
	}
	return p.Orch.DNSSyncError()
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
		msg, ok := render.Refusal(actErr)
		if !ok {
			return middleware.HTTPError(actErr)
		}
		v.Error, v.Note, status = msg, "", http.StatusUnprocessableEntity
	}
	ds, err := p.Orch.DNSDomains(ctx, org, id)
	if err != nil {
		msg, ok := render.Refusal(err)
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

