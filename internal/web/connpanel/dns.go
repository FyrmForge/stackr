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
	ui "github.com/FyrmForge/stackr/internal/ui/dns"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// The DNS capability's section: a connector's domain list and its actions,
// the same for every DNS kind.

// MountDNS adds the DNS section's actions under prefix behind gate.
func (p Panel) MountDNS(g *echo.Group, prefix string, gate echo.MiddlewareFunc) {
	g.POST(prefix+"/:connector/domains", p.domains, gate)
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

// POST <dns>/:connector/domains: the list, asked of the provider.
func (p Panel) domains(c echo.Context) error { return p.answer(c, "", nil) }

// POST <dns>/:connector/manage (domain, scope=all|only, org...): manage
// it, or set who may use it (all: every org, as none ticked is).
func (p Panel) manage(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	orgs := form["org"]
	if form.Get("scope") == "all" {
		orgs = nil
	}
	_, err = p.Orch.ManageDNSDomain(c.Request().Context(), p.Org(c), c.Param("connector"), form.Get("domain"), orgs)
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
	v := ui.DomainsView{ID: id, Provider: label, Shared: org == nil, Note: note}
	status := http.StatusOK
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
