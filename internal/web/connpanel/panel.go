// Package connpanel is the connector screens' handlers, shared by the
// admin drawer (server connectors), the org drawer (the org's own), the
// canvas's create button and the setup wizards. The add dialog is drawn
// from the kind's declared steps; the settings dialog has the core's tabs
// (Connection, Used by) and one per capability section (DNS in dns.go, Git
// in git.go). A level mounts them under its own base and gates; Org says
// whose connectors a request reaches.
package connpanel

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

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
// a GET whose answer holds the connectors list, fetched as a dialog
// closes; Return the page a redirect setup comes back to (nil = Refresh);
// Delete a connector's Remove (Action "" = none); Drawer, set, is where a
// row of a git connection opens instead of the settings dialog (the org's
// connector drawer).
type Panel struct {
	Orch    *service.Orchestrator
	Org     func(c echo.Context) *string
	Base    func(c echo.Context) string
	DNSBase func(c echo.Context) string
	Back    func(c echo.Context) string
	Refresh func(c echo.Context) string
	Return  func(c echo.Context) string
	Delete  func(c echo.Context, conn service.Connector) comp.ConfirmView
	Drawer  func(c echo.Context, conn service.Connector) string
	Only    string // the capability the level lists ("" = every connector)
}

// Mount adds the kind screens under prefix (the level's base pattern),
// each kind behind add(kind) (nil = gate), and the settings dialog and its
// actions behind gate; MountDNS the DNS section. Remove is the level's to
// mount.
func (p Panel) Mount(g *echo.Group, prefix string, gate echo.MiddlewareFunc, add func(service.ConnectorKind) echo.MiddlewareFunc) {
	for _, k := range p.Orch.ConnectorKinds() {
		kg := gate
		if add != nil {
			kg = add(k)
		}
		g.GET(prefix+"/new/"+k.Name, p.form(k.Name), kg)
		g.POST(prefix+"/new/"+k.Name, p.add(k.Name), kg)
	}
	g.GET(prefix+"/:connector/settings", p.Settings, gate)
	g.POST(prefix+"/:connector/check", p.check, gate)
	g.POST(prefix+"/:connector/rename", p.rename, gate)
	g.POST(prefix+"/:connector/credentials", p.update, gate)
	p.mountGit(g, prefix, gate)
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

// KindsWith is the registered kinds with capability cp.
func (p Panel) KindsWith(cp string) []service.ConnectorKind {
	var out []service.ConnectorKind
	for _, k := range p.Orch.ConnectorKinds() {
		if slices.Contains(k.Caps, cp) {
			out = append(out, k)
		}
	}
	return out
}

func hasDNS(k service.ConnectorKind) bool { return slices.Contains(k.Caps, service.CapDNS) }

// formSteps is what the add dialog's form shows: the kind's steps up to
// its first redirect (the screen that sends the browser out), or its Alt
// form's.
func formSteps(k service.ConnectorKind, alt bool) []service.ConnectorStep {
	if alt && k.Alt != nil {
		return k.Alt.Steps
	}
	for i, s := range k.Steps {
		if s.Out {
			return k.Steps[:i+1]
		}
	}
	return k.Steps
}

// total is every step of k's setup: its own, then each capability's.
func total(k service.ConnectorKind, alt bool) int {
	n := len(k.Steps)
	if alt && k.Alt != nil {
		n = len(k.Alt.Steps)
	}
	if hasDNS(k) {
		n++
	}
	return n
}

// ret is where a redirect setup begun from this request comes back to: the
// page that opened the dialog (?return=, a panel path), else the level's.
func (p Panel) ret(c echo.Context) string {
	if r := c.QueryParam("return"); r != "" {
		return r
	}
	if r := c.FormValue("return"); r != "" {
		return r
	}
	if p.Return != nil {
		return p.Return(c)
	}
	return p.Refresh(c)
}

// fieldsOf is a step's fields as inputs, vals in them (nil = each one's
// prefill); a secret is never filled. vals set without errs is a saved
// connection's form: its secrets are stored, shown as dots. A field with a
// fact to prefill and a value is read-only until the admin asks to type it.
func (p Panel) fieldsOf(c echo.Context, fs []service.ConnectorField, vals, errs map[string]string) []connectors.Field {
	var out []connectors.Field
	for _, f := range fs {
		fv := comp.FieldView{
			Name: f.Name, Label: f.Label, Help: f.Help, Required: !f.Optional,
			Error: comp.GetError(errs, f.Name), Autocomplete: "off",
		}
		cf := connectors.Field{Multiline: f.Multiline}
		switch {
		case f.Secret && !f.Multiline:
			fv.Type = "password"
		case f.Secret:
		case vals == nil:
			fv.Value = p.Orch.ConnectorPrefill(c.Request().Context(), f)
		default:
			fv.Value = vals[f.Name]
		}
		if f.Secret && vals != nil && errs == nil {
			cf.Stored, fv.Required = true, false
		}
		cf.Fixed = f.Prefill != "" && fv.Value != "" && fv.Error == ""
		cf.FieldView = fv
		out = append(out, cf)
	}
	return out
}

// view is k's screens with vals in the fields (nil = each one's prefill)
// and the refusals in errs.
func (p Panel) view(c echo.Context, k service.ConnectorKind, alt bool, vals, errs map[string]string) connectors.SetupView {
	post := p.Base(c) + "/new/" + k.Name
	v := connectors.SetupView{
		Post: post, Total: total(k, alt), Errors: errs, Connect: "Connect",
		Hidden: map[string]string{"return": p.ret(c)},
	}
	if k.Redirects && !alt {
		v.Connect = "Next"
	}
	if p.Back != nil {
		v.Back = p.Back(c)
	}
	back := "?return=" + url.QueryEscape(v.Hidden["return"])
	switch {
	case alt:
		v.Hidden["alt"] = "1"
		v.Back = post + back
	case k.Alt != nil:
		v.Alt, v.AltLabel = post+back+"&alt=1", k.Alt.Label
	}
	for _, s := range formSteps(k, alt) {
		v.Steps = append(v.Steps, connectors.Step{
			Title: s.Title, Intro: s.Intro, List: s.List, Note: s.Note,
			Fields: p.fieldsOf(c, s.Fields, vals, errs),
		})
	}
	return v
}

// Open is the add dialog opened straight on kind (a wizard's, the canvas's).
func (p Panel) Open(c echo.Context, k service.ConnectorKind) templ.Component {
	return connectors.Modal("Add a "+k.Label+" connector", p.Refresh(c), connectors.Setup(p.view(c, k, false, nil, nil)))
}

// GET <base>/new/<kind> (?alt=1 its second way, ?modal=1 a whole dialog,
// ?return= where a redirect setup comes back to): the kind's screens.
func (p Panel) form(kind string) echo.HandlerFunc {
	return func(c echo.Context) error {
		k, err := p.Orch.ConnectorKindOf(kind)
		if err != nil {
			return middleware.HTTPError(err)
		}
		body := connectors.Setup(p.view(c, k, c.QueryParam("alt") != "", nil, nil))
		if c.QueryParam("modal") != "" {
			return respond.HTML(c, http.StatusOK, connectors.Modal("Add a "+k.Label+" connector", p.Refresh(c), body))
		}
		return respond.HTML(c, http.StatusOK, body)
	}
}

// POST <base>/new/<kind> (the kind's fields, alt, return): check and save
// it, then its capability's screen; or for a redirect setup the screen
// that sends the browser to the provider. A refusal shows the screens
// again on the step with the first error, every value kept but a secret's.
func (p Panel) add(kind string) echo.HandlerFunc {
	return func(c echo.Context) error {
		k, err := p.Orch.ConnectorKindOf(kind)
		if err != nil {
			return middleware.HTTPError(err)
		}
		form, err := c.FormParams()
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "bad form")
		}
		alt := form.Get("alt") != "" && k.Alt != nil
		vals := map[string]string{}
		for _, s := range formSteps(k, alt) {
			for _, f := range s.Fields {
				vals[f.Name] = form.Get(f.Name)
			}
		}
		ctx := c.Request().Context()
		a, err := p.Orch.AddConnector(ctx, service.ConnectorAdd{
			OrgID: p.Org(c), Kind: k.Name, Alt: alt, Values: vals,
			User: middleware.Principal(c).User.ID, Return: p.ret(c),
		})
		if err == nil {
			v := connectors.SetupView{Total: total(k, alt)}
			id := a.Connector.ID
			switch {
			case a.Out != nil:
				i := len(formSteps(k, false)) - 1
				v.Out = &connectors.OutView{
					Step: i + 1, Title: k.Steps[i].Title, Intro: k.Steps[i].Intro,
					Method: a.Out.Method, URL: a.Out.URL, Values: a.Out.Values, Label: k.Label,
				}
			case hasDNS(k):
				v.Done = &connectors.DoneView{Step: v.Total, Title: dnsui.Title, Body: dnsui.Connected(id, p.DNSBase(c)+"/"+id+"/domains", k.Label)}
			case slices.Contains(k.Caps, service.CapGit):
				gv, err := p.gitView(c, a.Connector)
				if err != nil {
					return middleware.HTTPError(err)
				}
				v.Done = &connectors.DoneView{Step: v.Total, Title: "Connected", Body: connectors.Git(gv)}
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
		return respond.HTML(c, http.StatusUnprocessableEntity, connectors.Setup(p.view(c, k, alt, vals, errs)))
	}
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

func (p Panel) label(k service.Connector) string {
	if kd, err := p.Orch.ConnectorKindOf(k.Kind); err == nil {
		return kd.Label
	}
	return k.Kind
}

// Rows is the level's connectors in the list, each summed up by its
// capabilities: a pending one waits, one whose last check failed says so.
func (p Panel) Rows(c echo.Context) ([]connectors.Row, error) {
	ctx, org := c.Request().Context(), p.Org(c)
	cs, err := p.Orch.ConnectorRows(ctx, org)
	if err != nil {
		return nil, err
	}
	n, err := p.Orch.DNSManaged(ctx, org)
	if err != nil {
		return nil, err
	}
	shares, err := p.shares(c)
	if err != nil {
		return nil, err
	}
	var out []connectors.Row
	for _, k := range cs {
		if p.Only != "" && !slices.Contains(k.Capabilities, p.Only) {
			continue
		}
		r := connectors.Row{
			ID: k.ID, Kind: p.label(k), Name: k.Name, Pending: k.Pending, Status: k.CheckError,
		}
		if slices.Contains(k.Capabilities, service.CapDNS) {
			r.Summary = managed(n[k.ID])
		}
		if slices.Contains(k.Capabilities, service.CapGit) {
			if err := p.gitRow(c, k, shares, &r); err != nil {
				return nil, err
			}
		}
		if h, ok := strings.CutPrefix(r.Detail, "Webhook URL "); ok {
			r.Hook, r.Detail = h, ""
		}
		settings := p.Base(c) + "/" + k.ID + "/settings"
		switch {
		case r.Drawer != "":
			r.Actions = []connectors.Action{{Label: "Manage", URL: r.Drawer, Drawer: true, Push: r.Push}}
		case k.Pending:
			r.Actions = []connectors.Action{{Label: "Settings", URL: settings}}
		case slices.Contains(k.Capabilities, service.CapDNS):
			r.Actions = []connectors.Action{{Label: "Manage domains", URL: settings + "?tab=domains"}, {Label: "Settings", URL: settings + "?tab=connection"}}
		case slices.Contains(k.Capabilities, service.CapGit) && p.Org(c) == nil:
			r.Actions = []connectors.Action{{Label: "Sharing", URL: settings + "?tab=git"}, {Label: "Settings", URL: settings + "?tab=connection"}}
		default:
			r.Actions = []connectors.Action{{Label: "Settings", URL: settings}}
		}
		if p.Delete != nil {
			r.Remove = p.Delete(c, k)
		}
		out = append(out, r)
	}
	return out, nil
}

// Warning is what removing connection k does, by its capabilities.
func Warning(k service.Connector) string {
	switch {
	case slices.Contains(k.Capabilities, service.CapDNS):
		return "stackr deletes the DNS records it made and stops managing its domains. Refused while a tile uses one."
	case slices.Contains(k.Capabilities, service.CapGit):
		return "Organizations and stacks bound to it must be unbound first. It stays installed at the provider; uninstall it there too."
	}
	return "stackr forgets the account. Nothing changes at the provider."
}
