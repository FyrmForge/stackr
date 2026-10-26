package connpanel

import (
	"fmt"
	"net/http"
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

// Synced is the last records sync's failure; it can name any org's
// hosts, so only the server level shows it.
func (p Panel) Synced(c echo.Context) string {
	if p.Org(c) != nil {
		return ""
	}
	return p.Orch.DNSSyncError()
}

// POST <dns>/:connector/domains (not_now): the list, asked of the
// provider; not_now leaves the setup suggestion out.
func (p Panel) domains(c echo.Context) error { return p.answer(c, "", nil) }

// under is the scope a form asks for: the name under the domain typed in
// under, unless part is all ("" = the whole domain).
func under(c echo.Context, domain string) string {
	sub := strings.TrimSpace(c.FormValue("under"))
	if c.FormValue("part") == "all" || sub == "" {
		return ""
	}
	return service.ManagedHost(domain, sub)
}

// POST <dns>/:connector/preview (domain, part, under): the confirm before
// managing it, listing the records stackr writes, the existing ones it may
// adopt and the ones it leaves alone outside the scope. With none of them,
// there is nothing to ask: it manages the domain.
func (p Panel) preview(c echo.Context) error {
	domain := c.FormValue("domain")
	base := p.DNSBase(c) + "/" + c.Param("connector")
	v := &ui.PlanView{Domain: domain, Preview: base + "/preview", Manage: base + "/manage"}
	if under(c, domain) != "" {
		v.Under = strings.TrimSpace(c.FormValue("under"))
	}
	plan, err := p.Orch.PlanManageDNSDomain(c.Request().Context(), p.Org(c), c.Param("connector"), domain, under(c, domain), nil)
	if bad, ok := errs.IsInvalid(err); ok && bad.Field == "under" {
		v.UnderError, v.Manage = bad.Msg, ""
		return p.list(c, "", nil, v)
	}
	if err != nil {
		return p.answer(c, "", err)
	}
	if len(plan.Writes) == 0 && len(plan.Adopt) == 0 && len(plan.LeftAlone) == 0 {
		return p.manage(c)
	}
	v.Scope = plan.Scope
	for _, w := range plan.Writes {
		v.Writes = append(v.Writes, ui.PlanWrite{Host: w.Host, Type: w.Type, Value: w.Value})
	}
	for _, w := range plan.LeftAlone {
		v.LeftAlone = append(v.LeftAlone, ui.PlanWrite{Host: w.Host, Type: w.Type, Value: w.Value})
	}
	for _, a := range plan.Adopt {
		v.Adopt = append(v.Adopt, ui.PlanAdopt{
			Host: a.Host, Type: a.Type, Address: a.Value, On: a.Serves,
			Value: strings.Join([]string{a.Name, a.Type, a.Value}, "|"),
		})
	}
	return p.list(c, "", nil, v)
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
	var adopt []service.DNSRef
	for _, a := range form["adopt"] {
		if f := strings.SplitN(a, "|", 3); len(f) == 3 {
			adopt = append(adopt, service.DNSRef{Name: f[0], Type: f[1], Value: f[2]})
		}
	}
	_, err = p.Orch.ManageDNSDomainAdopting(c.Request().Context(), p.Org(c), c.Param("connector"), form.Get("domain"), under(c, form.Get("domain")), orgs, adopt)
	return p.answer(c, "Saved.", err)
}

func (p Panel) unmanage(c echo.Context) error {
	err := p.Orch.UnmanageDNSDomain(c.Request().Context(), p.Org(c), c.Param("connector"), c.FormValue("domain"))
	return p.answer(c, "No longer managed.", err)
}

func (p Panel) takeover(c echo.Context) error {
	err := p.Orch.TakeOverDNSDomain(c.Request().Context(), p.Org(c), c.Param("connector"), c.FormValue("domain"), c.FormValue("confirm"))
	return p.answer(c, "Switched to %s DNS.", err)
}

// answer is the list again, the action's refusal or note on top (%s in a
// note is the provider). A provider that does not answer is the list's
// text, not an error page.
func (p Panel) answer(c echo.Context, note string, actErr error) error {
	return p.list(c, note, actErr, nil)
}

// list is answer with a manage confirm on top when plan is set.
func (p Panel) list(c echo.Context, note string, actErr error, plan *ui.PlanView) error {
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
	if plan != nil && plan.UnderError != "" {
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
	if c.FormValue("not_now") == "" && plan == nil {
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
