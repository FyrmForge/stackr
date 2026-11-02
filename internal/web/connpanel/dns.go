package connpanel

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	ui "github.com/FyrmForge/stackr/internal/ui/dns"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// The DNS capability's section: a connector's domain list and its actions,
// the same for every DNS kind.

// MountDNS adds the DNS section's actions under prefix behind gate.
func (p Panel) MountDNS(g *echo.Group, prefix string, gate echo.MiddlewareFunc) {
	g.POST(prefix+"/:connector/domains", p.domains, gate)
	g.POST(prefix+"/:connector/preview", p.preview, gate)
	g.POST(prefix+"/:connector/manage", p.manage, gate)
	g.POST(prefix+"/:connector/rescope-preview", p.rescopePreview, gate)
	g.POST(prefix+"/:connector/rescope", p.rescope, gate)
	g.POST(prefix+"/:connector/records", p.openRecords, gate)
	g.POST(prefix+"/:connector/record-add", p.addRecord, gate)
	g.POST(prefix+"/:connector/record-rm", p.removeRecord, gate)
	g.POST(prefix+"/:connector/unmanage", p.unmanage, gate)
	g.POST(prefix+"/:connector/takeover", p.takeover, gate)
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

// Synced is the last records sync's failure, at an org level only the
// parts about its own connectors' domains.
func (p Panel) Synced(c echo.Context) string {
	return p.Orch.DNSSyncErrorFor(c.Request().Context(), p.Org(c))
}

// POST <dns>/:connector/domains (not_now): the list, asked of the
// provider; not_now leaves the setup suggestion out. This list and a
// manage, unmanage or take over that worked (done) offer it.
func (p Panel) domains(c echo.Context) error {
	return p.list(c, "", nil, nil, nil, c.FormValue("not_now") == "")
}

// under is the scope a form asks for: the name under the domain typed in
// under (the full name pasted reads the same), unless part is all ("" =
// the whole domain).
func under(c echo.Context, domain string) string {
	if c.FormValue("part") == "all" {
		return ""
	}
	return service.DNSScope(domain, c.FormValue("under"))
}

// POST <dns>/:connector/preview (domain, part, under): the confirm before
// managing it, listing the records stackr writes, the existing ones it may
// adopt and the ones it leaves alone outside the scope. With none of them,
// there is nothing to ask: it manages the domain.
func (p Panel) preview(c echo.Context) error {
	domain := c.FormValue("domain")
	base := p.DNSBase(c) + "/" + c.Param("connector")
	v := planView(c, domain, base+"/preview", base+"/manage")
	plan, err := p.Orch.PlanManageDNSDomain(c.Request().Context(), p.Org(c), c.Param("connector"), domain, under(c, domain), nil)
	if bad, ok := errs.IsInvalid(err); ok && bad.Field == "under" {
		v.UnderError, v.Manage = bad.Msg, ""
		return p.list(c, "", nil, v, nil, false)
	}
	if err != nil {
		return p.answer(c, "", err)
	}
	v.Scope = plan.Scope
	v.Writes, v.LeftAlone, v.Adopt = planWrites(plan.Writes), planWrites(plan.LeftAlone), planAdopts(plan.Adopt)
	v.Nothing = len(plan.Writes) == 0 && len(plan.Adopt) == 0 && len(plan.LeftAlone) == 0
	return p.list(c, "", nil, v, nil, false)
}

// POST <dns>/:connector/rescope-preview (domain, part, under): the confirm
// before changing a managed domain's scope, listing the records handed
// back, the ones stackr writes and the ones it may adopt.
func (p Panel) rescopePreview(c echo.Context) error {
	domain := c.FormValue("domain")
	base := p.DNSBase(c) + "/" + c.Param("connector")
	v := planView(c, domain, base+"/rescope-preview", base+"/rescope")
	v.Rescope = true
	plan, err := p.Orch.PlanRescopeDNSDomain(c.Request().Context(), p.Org(c), c.Param("connector"), domain, under(c, domain))
	if bad, ok := errs.IsInvalid(err); ok && bad.Field == "under" {
		v.UnderError, v.Manage = bad.Msg, ""
		return p.list(c, "", nil, v, nil, false)
	}
	if err != nil {
		return p.answer(c, "", err)
	}
	v.Scope, v.Resource, v.From = plan.Scope, plan.Resource, plan.From
	v.HandBack, v.Writes, v.Adopt = planWrites(plan.HandBack), planWrites(plan.Writes), planAdopts(plan.Adopt)
	v.Nothing = len(plan.HandBack) == 0 && len(plan.Writes) == 0 && len(plan.Adopt) == 0 && plan.Resource == ""
	return p.list(c, "", nil, v, nil, false)
}

// planView is a confirm's scope form for domain as the form asks it.
func planView(c echo.Context, domain, preview, act string) *ui.PlanView {
	v := &ui.PlanView{Domain: domain, Preview: preview, Manage: act}
	if u := under(c, domain); u != "" {
		v.Under = strings.TrimSuffix(u, "."+strings.ToLower(strings.Trim(strings.TrimSpace(domain), ".")))
	}
	return v
}

func planWrites(ws []service.DNSWrite) []ui.PlanWrite {
	var out []ui.PlanWrite
	for _, w := range ws {
		out = append(out, ui.PlanWrite{Host: w.Host, Type: w.Type, Value: w.Value})
	}
	return out
}

func planAdopts(as []service.DNSAdopt) []ui.PlanAdopt {
	var out []ui.PlanAdopt
	for _, a := range as {
		out = append(out, ui.PlanAdopt{
			Host: a.Host, Type: a.Type, Address: a.Value, On: a.Serves,
			Value: strings.Join([]string{a.Name, a.Type, a.Value}, "|"),
		})
	}
	return out
}

// adoptOf is the ticked adopt values (name|type|value) of form.
func adoptOf(form url.Values) []service.DNSRef {
	var out []service.DNSRef
	for _, a := range form["adopt"] {
		if f := strings.SplitN(a, "|", 3); len(f) == 3 {
			out = append(out, service.DNSRef{Name: f[0], Type: f[1], Value: f[2]})
		}
	}
	return out
}

// POST <dns>/:connector/manage (domain, part, under, scope=all|only, org...,
// adopt...): manage it, or set who may use it (all: every org, as none
// ticked is). part and under are what to manage, as preview reads them;
// adopt is name|type|value of the existing records to take over.
func (p Panel) manage(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	orgs := form["org"]
	if form.Get("scope") == "all" {
		orgs = nil
	}
	_, err = p.Orch.ManageDNSDomainAdopting(c.Request().Context(), p.Org(c), c.Param("connector"), form.Get("domain"), under(c, form.Get("domain")), orgs, adoptOf(form))
	return p.done(c, "Saved.", err)
}

// POST <dns>/:connector/rescope (domain, part, under, from, adopt...):
// change what part of a managed domain stackr manages. from is the scope
// the confirm was planned from.
func (p Panel) rescope(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	var from *string
	if form.Has("from") {
		f := form.Get("from")
		from = &f
	}
	_, err = p.Orch.RescopeDNSDomain(c.Request().Context(), p.Org(c), c.Param("connector"), form.Get("domain"), under(c, form.Get("domain")), from, adoptOf(form))
	return p.answer(c, "Scope changed.", err)
}

// POST <dns>/:connector/records (domain): openRecords is the list with the domain's
// custom records open under its row.
func (p Panel) openRecords(c echo.Context) error { return p.withRecords(c, "", nil) }

// POST <dns>/:connector/record-add (domain, rec_name, rec_type, rec_value).
func (p Panel) addRecord(c echo.Context) error {
	r, err := p.Orch.AddDNSDomainRecord(c.Request().Context(), p.Org(c), c.Param("connector"), c.FormValue("domain"), RecordIn(c))
	return p.withRecords(c, RecordNote(r), err)
}

// POST <dns>/:connector/record-rm (domain, record).
func (p Panel) removeRecord(c echo.Context) error {
	err := p.Orch.RemoveDNSDomainRecord(c.Request().Context(), p.Org(c), c.Param("connector"), c.FormValue("domain"), c.FormValue("record"))
	return p.withRecords(c, "Record removed.", err)
}

// withRecords is the list with the posted domain's records open, the
// action's refusal or note in them.
func (p Panel) withRecords(c echo.Context, note string, actErr error) error {
	id, domain := c.Param("connector"), c.FormValue("domain")
	rs, err := p.Orch.DNSDomainRecords(c.Request().Context(), p.Org(c), id, domain)
	if err != nil {
		return p.answer(c, "", err)
	}
	base := p.DNSBase(c) + "/" + id
	v := Records(rs, domain, base+"/record-add", base+"/record-rm", ui.ListTarget(id))
	v.Vals = map[string]string{"domain": domain}
	v.Note = note
	if actErr != nil {
		msg, ok := render.Refuse(c, "", actErr)
		if !ok {
			return middleware.HTTPError(actErr)
		}
		v.Error, v.Note = msg, ""
	}
	return p.list(c, "", nil, nil, &v, false)
}

// RecordIn is the posted add record form.
func RecordIn(c echo.Context) service.CustomRecordIn {
	return service.CustomRecordIn{Name: c.FormValue("rec_name"), Type: c.FormValue("rec_type"), Value: c.FormValue("rec_value")}
}

// RecordNote says how an add landed.
func RecordNote(r service.CustomRecord) string {
	if r.Note != "" {
		return "Record " + r.Note + "."
	}
	return "Record added."
}

// Records is an owner's custom records as the shared section draws them:
// key the owner, add and remove the actions, target what they answer.
func Records(rs service.CustomRecords, key, add, remove, target string) ui.RecordsView {
	v := ui.RecordsView{Key: key, Add: add, Remove: remove, Target: target}
	if rs.Source != "" {
		v.Source = strings.ToUpper(rs.Source[:1]) + rs.Source[1:]
	}
	for _, r := range rs.Records {
		row := ui.RecordRow{ID: r.ID, Host: r.Host, Type: r.Type, Value: r.Value, PutBack: r.PutBackAt != nil}
		if r.NotLive != "" {
			row.NotLive = "Not live: " + strings.ToLower(r.NotLive[:1]) + r.NotLive[1:]
		}
		v.Rows = append(v.Rows, row)
	}
	return v
}

func (p Panel) unmanage(c echo.Context) error {
	err := p.Orch.UnmanageDNSDomain(c.Request().Context(), p.Org(c), c.Param("connector"), c.FormValue("domain"))
	return p.done(c, "No longer managed.", err)
}

func (p Panel) takeover(c echo.Context) error {
	err := p.Orch.TakeOverDNSDomain(c.Request().Context(), p.Org(c), c.Param("connector"), c.FormValue("domain"), c.FormValue("confirm"))
	return p.done(c, "Switched to %s DNS.", err)
}

// answer is the list again, the action's refusal or note on top (%s in a
// note is the provider). A provider that does not answer is the list's
// text, not an error page.
func (p Panel) answer(c echo.Context, note string, actErr error) error {
	return p.list(c, note, actErr, nil, nil, false)
}

// done is answer after a manage, unmanage or take over: once it worked,
// the next setup suggestion comes with it.
func (p Panel) done(c echo.Context, note string, actErr error) error {
	return p.list(c, note, actErr, nil, nil, actErr == nil)
}

// list is answer with a manage confirm on top when plan is set, and the
// setup suggestion when suggest.
func (p Panel) list(c echo.Context, note string, actErr error, plan *ui.PlanView, recs *ui.RecordsView, suggest bool) error {
	ctx, id := c.Request().Context(), c.Param("connector")
	org := p.Org(c)
	k, err := p.Orch.ConnectorAt(ctx, org, id)
	if err != nil {
		return middleware.HTTPError(err)
	}
	label := p.label(k)
	if strings.Contains(note, "%s") {
		note = fmt.Sprintf(note, label)
	}
	base := p.DNSBase(c) + "/" + id
	v := ui.DomainsView{ID: id, Provider: label, Shared: org == nil, Note: note, Plan: plan}
	status := http.StatusOK
	if plan != nil && plan.UnderError != "" || recs != nil && recs.Error != "" {
		status = http.StatusUnprocessableEntity
	}
	if actErr != nil {
		msg, ok := render.Refusal(actErr)
		if !ok {
			return middleware.HTTPError(actErr)
		}
		v.Error, v.Note, status = msg, "", http.StatusUnprocessableEntity
		v.InUse = InUse(actErr)
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
	if suggest {
		for _, s := range p.Orch.DNSSuggestions(ctx, org, ds) {
			v.Suggest = append(v.Suggest, ui.Suggestion{
				Domain: s.Domain, Host: s.Host, Sub: s.Sub, Panel: s.Panel,
				Preview: base + "/preview", NotNow: base + "/domains",
			})
		}
	}
	var orgs []service.Org
	if org == nil {
		if orgs, err = p.Orch.AllOrgs(ctx); err != nil {
			return middleware.HTTPError(err)
		}
	}
	// managed first, as stable as the provider's own order
	slices.SortStableFunc(ds, func(a, b service.DNSDomain) int {
		return cmpBool(b.State == service.DNSManaged, a.State == service.DNSManaged)
	})
	for _, d := range ds {
		r := ui.DomainRow{
			Name:     d.Name,
			State:    d.State,
			Manage:   base + "/manage",
			Preview:  base + "/preview",
			Unmanage: base + "/unmanage",
			TakeOver: base + "/takeover",
			All:      d.ShareAll,
			Scope:    d.Scope,
		}
		if d.State == service.DNSManaged {
			r.Rescope, r.Records = base+"/rescope-preview", base+"/records"
			if recs != nil && recs.Key == d.Name {
				r.Open = recs
			}
		}
		for _, og := range orgs {
			r.Orgs = append(r.Orgs, ui.Org{ID: og.ID, Name: og.Name, On: !d.ShareAll && slices.Contains(d.OrgIDs, og.ID)})
		}
		v.Rows = append(v.Rows, r)
	}
	return respond.HTML(c, status, ui.Domains(v))
}

// cmpBool orders true before false.
func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}
