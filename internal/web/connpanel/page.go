package connpanel

import (
	"net/url"
	"strings"

	"github.com/FyrmForge/stackr/internal/service"
	dnsui "github.com/FyrmForge/stackr/internal/ui/dns"
)

// ManagedPage is the Domains page's view of ds. A domain the viewer may
// edit links its connector's Change scope and records: on the server's
// Connectors section (serverConns) or the owning org's Connectors tab.
func ManagedPage(ds []service.ManagedDomain, serverConns string) dnsui.PageView {
	var v dnsui.PageView
	for _, d := range ds {
		pd := dnsui.PageDomain{Name: d.Name, Scope: d.ScopeText, Via: d.Via}
		switch {
		case d.AllOrgs:
			pd.Who = "every org"
		case len(d.Orgs) > 0:
			pd.Who = strings.Join(d.Orgs, ", ")
		}
		if d.CanEdit {
			page, q := serverConns+"?", url.Values{"conn": {d.ConnectorID}, "domain": {d.Name}}
			if d.ConnectorOrgID != "" {
				page = "/" + d.ConnectorOrg + "?drawer=org:" + d.ConnectorOrgID + "&tab=connectors&"
			}
			q.Set("act", "records")
			pd.Records = page + q.Encode()
			q.Set("act", "rescope")
			if d.Scope != "" {
				q.Set("under", strings.TrimSuffix(d.Scope, "."+d.Name))
			}
			pd.Rescope = page + q.Encode()
		}
		for _, r := range d.Records {
			pr := dnsui.PageRecord{Name: r.Name, Type: r.Type, Value: r.Value, Whose: r.WhoseText, Drift: r.Drift}
			if r.Host != r.Name {
				pr.Host = r.Host
			}
			if r.Tile != nil && r.Whose == service.WhoseApp {
				pr.Tile, pr.Href = r.Tile.Path, Href(*r.Tile)+"&tab=domains"
			}
			pd.Rows = append(pd.Rows, pr)
		}
		v.Domains = append(v.Domains, pd)
	}
	return v
}
