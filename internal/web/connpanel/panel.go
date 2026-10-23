// Package connpanel is the connector screens' handlers, shared by the
// admin drawer (server connectors), the org drawer (the org's own) and the
// setup wizard's Certificates step. The add dialog is drawn from the kind's
// declared steps; the settings dialog has one section per capability (the
// DNS one in dns.go). A level mounts them under its own base and gate; Org
// says whose connectors a request reaches. GitHub stays on its own screens
// until it moves onto the Git source capability.
package connpanel

import (
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
	dnsui "github.com/FyrmForge/stackr/internal/ui/dns"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// Panel is one level. Base is where the add dialog's kind screens and a
// connector's settings hang, DNSBase the DNS section's actions; Back is
// the add dialog's kind picker (nil = opened straight on a kind); Refresh
// a GET whose answer holds the connectors list, fetched as a dialog closes
// and where a redirect setup comes back to; Delete a connector's Remove
// (Action "" = none).
type Panel struct {
	Orch    *service.Orchestrator
	Org     func(c echo.Context) *string
	Base    func(c echo.Context) string
	DNSBase func(c echo.Context) string
	Back    func(c echo.Context) string
	Refresh func(c echo.Context) string
	Delete  func(c echo.Context, conn service.Connector) comp.ConfirmView
	Only    string // the capability the level lists ("" = every connector)
}

// Mount adds the kind screens under prefix (the level's base pattern)
// behind gate; MountDNS the DNS section. Settings and Remove
// are the level's to mount: Admin shares those paths with GitHub.
func (p Panel) Mount(g *echo.Group, prefix string, gate echo.MiddlewareFunc) {
	g.GET(prefix+"/new/:kind", p.form, gate)
	g.POST(prefix+"/new/:kind", p.add, gate)
}

// Kinds is the add dialog's picker entries for the registered kinds, those
// with capability cp ("" = every kind).
func (p Panel) Kinds(c echo.Context, cp string) []connectors.Kind {
	var out []connectors.Kind
	for _, k := range p.Orch.ConnectorKinds() {
		if cp == "" || slices.Contains(k.Caps, cp) {
			out = append(out, connectors.Kind{Name: k.Label, Hint: k.Hint, URL: p.Base(c) + "/new/" + k.Name})
		}
	}
	return out
}

func hasDNS(k service.ConnectorKind) bool { return slices.Contains(k.Caps, service.CapDNS) }

// total is every step of k's setup: its own, then each capability's.
func total(k service.ConnectorKind) int {
	n := len(k.Steps)
	if hasDNS(k) {
		n++
	}
	return n
}

// view is k's screens with vals in the fields (nil = each one's prefill)
// and the refusals in errs. A secret field is never filled.
func (p Panel) view(c echo.Context, k service.ConnectorKind, vals, errs map[string]string) connectors.SetupView {
	v := connectors.SetupView{Post: p.Base(c) + "/new/" + k.Name, Total: total(k), Errors: errs, Connect: "Connect"}
	if k.Redirects {
		v.Connect = "Next"
	}
	if p.Back != nil {
		v.Back = p.Back(c)
	}
	for _, s := range k.Steps {
		st := connectors.Step{Title: s.Title, Intro: s.Intro, List: s.List, Note: s.Note}
		for _, f := range s.Fields {
			fv := comp.FieldView{
				Name: f.Name, Label: f.Label, Help: f.Help, Required: !f.Optional,
				Error: comp.GetError(errs, f.Name), Autocomplete: "off",
			}
			switch {
			case f.Secret:
				fv.Type = "password"
			case vals == nil:
				fv.Value = p.Orch.ConnectorPrefill(c.Request().Context(), f)
			default:
				fv.Value = vals[f.Name]
			}
			st.Fields = append(st.Fields, fv)
		}
		v.Steps = append(v.Steps, st)
	}
	return v
}

func (p Panel) kind(c echo.Context) (service.ConnectorKind, error) {
	return p.Orch.ConnectorKindOf(c.Param("kind"))
}

// Open is the add dialog opened straight on kind (the wizard's).
func (p Panel) Open(c echo.Context, k service.ConnectorKind) templ.Component {
	return connectors.Modal("Add a "+k.Label+" connector", p.Refresh(c), connectors.Setup(p.view(c, k, nil, nil)))
}

// GET <base>/new/:kind: the kind's screens, into the add dialog.
func (p Panel) form(c echo.Context) error {
	k, err := p.kind(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, http.StatusOK, connectors.Setup(p.view(c, k, nil, nil)))
}

// POST <base>/new/:kind (the kind's fields): check and save it, then its
// capability's screen; or for a redirect setup the screen that sends the
// browser to the provider. A refusal shows the screens again on the step
// with the first error, every value kept but a secret's.
func (p Panel) add(c echo.Context) error {
	k, err := p.kind(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	vals := map[string]string{}
	for _, s := range k.Steps {
		for _, f := range s.Fields {
			vals[f.Name] = form.Get(f.Name)
		}
	}
	a, err := p.Orch.AddConnector(c.Request().Context(), service.ConnectorAdd{
		OrgID: p.Org(c), Kind: k.Name, Values: vals,
		User: middleware.Principal(c).User.ID, Return: p.Refresh(c),
	})
	if err == nil {
		v := connectors.SetupView{Total: total(k)}
		switch {
		case a.Out != nil:
			last := k.Steps[len(k.Steps)-1]
			v.Out = &connectors.OutView{
				Step: len(k.Steps), Title: last.Title, Intro: last.Intro,
				Method: a.Out.Method, URL: a.Out.URL, Values: a.Out.Values, Label: k.Label,
			}
		case hasDNS(k):
			id := a.Connector.ID
			v.Done = &connectors.DoneView{Step: v.Total, Title: dnsui.Title, Body: dnsui.Connected(id, p.DNSBase(c)+"/"+id+"/domains", k.Label)}
		default:
			v.Done = &connectors.DoneView{Step: v.Total, Title: k.Steps[len(k.Steps)-1].Title}
		}
		return respond.HTML(c, http.StatusOK, connectors.Setup(v))
	}
	errs := fields(err, vals)
	if len(errs) == 0 {
		msg, ok := render.Refusal(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		errs["general"] = msg
	}
	return respond.HTML(c, http.StatusUnprocessableEntity, connectors.Setup(p.view(c, k, vals, errs)))
}

// fields is every field error in err, which may join several; one on a
// field the form does not have is the general message.
func fields(err error, form map[string]string) map[string]string {
	out := map[string]string{}
	var walk func(error)
	walk = func(e error) {
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, x := range j.Unwrap() {
				walk(x)
			}
			return
		}
		if inv, ok := errs.IsInvalid(e); ok {
			if _, on := form[inv.Field]; on {
				out[inv.Field] = inv.Msg
			} else {
				out["general"] = inv.Msg
			}
		}
	}
	walk(err)
	return out
}

// Settings (GET <base>/:connector/settings): the settings dialog, a section
// per capability and Remove.
func (p Panel) Settings(c echo.Context) error {
	k, err := p.Orch.ConnectorAt(c.Request().Context(), p.Org(c), c.Param("connector"))
	if err != nil {
		return middleware.HTTPError(err)
	}
	v := connectors.SettingsView{}
	if p.Delete != nil {
		v.Delete = p.Delete(c, k)
	}
	if slices.Contains(k.Capabilities, service.CapDNS) {
		v.Sections = append(v.Sections, dnsui.Load(k.ID, p.DNSBase(c)+"/"+k.ID+"/domains", p.label(k)))
	}
	return respond.HTML(c, http.StatusOK, connectors.Modal(k.Name, p.Refresh(c), connectors.Settings(v)))
}

func (p Panel) label(k service.Connector) string {
	if kd, err := p.Orch.ConnectorKindOf(k.Kind); err == nil {
		return kd.Label
	}
	return k.Kind
}

// Rows is the level's connectors in the list, each summed up by its
// capabilities.
func (p Panel) Rows(c echo.Context) ([]connectors.Row, error) {
	ctx, org := c.Request().Context(), p.Org(c)
	cs, err := p.Orch.ConnectorsAt(ctx, org)
	if err != nil {
		return nil, err
	}
	n, err := p.Orch.DNSManaged(ctx, org)
	if err != nil {
		return nil, err
	}
	var out []connectors.Row
	for _, k := range cs {
		if p.Only != "" && !slices.Contains(k.Capabilities, p.Only) {
			continue
		}
		r := connectors.Row{ID: k.ID, Kind: p.label(k), Name: k.Name, Settings: p.Base(c) + "/" + k.ID + "/settings"}
		if slices.Contains(k.Capabilities, service.CapDNS) {
			r.Summary = managed(n[k.ID])
		}
		out = append(out, r)
	}
	return out, nil
}

// Warning is what removing connection k does, by its capabilities.
func Warning(k service.Connector) string {
	if slices.Contains(k.Capabilities, service.CapDNS) {
		return "stackr deletes the DNS records it made and stops managing its domains. Refused while a tile uses one."
	}
	return "stackr forgets the account. Nothing changes at the provider."
}
