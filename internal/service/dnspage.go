package service

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// dnspage.go is the Domains page (RFC "Change scope and custom DNS
// records", Part 3): every managed domain and the records stackr wrote on
// it, from stackr's own table. Never a provider call, never a record stackr
// did not write.

// Whose a managed record is.
const (
	WhoseApp        = "app"         // written for a tile hostname: its A and its custom records
	WhoseDomain     = "domain"      // a custom record on the domain itself
	WhoseUnused     = "unused"      // written for a hostname no app wants any more
	WhosePanel      = "panel"       // the panel's own address
	WhoseNode       = "node"        // a node's own address
	WhoseHandedBack = "handed_back" // a narrowing gave it to the user; stackr no longer touches it
)

// whoseWords is how the screens say Whose.
var whoseWords = map[string]string{
	WhoseApp: "App", WhoseDomain: "Domain", WhoseUnused: "Unused",
	WhosePanel: "Panel", WhoseNode: "Node", WhoseHandedBack: "Handed back",
}

// ManagedRecord is one record stackr wrote on a managed domain. Name is
// the full host; Host the app hostname it serves and Tile that app (App
// and a handed-back app record only). Drift says what happened to it
// outside stackr ("" = nothing).
type ManagedRecord struct {
	Name         string     `json:"name"`
	Type         string     `json:"type"`
	Value        string     `json:"value"`
	Whose        string     `json:"whose"`
	WhoseText    string     `json:"whose_text"`
	Host         string     `json:"host,omitempty"`
	Tile         *Usage     `json:"tile,omitempty"`
	Drift        string     `json:"drift,omitempty"`
	PutBackAt    *time.Time `json:"put_back_at,omitempty"`
	HandedBackAt *time.Time `json:"handed_back_at,omitempty"`
}

// ManagedDomain is one managed domain as the Domains page shows it. Scope
// is the name managed at and under ("" = all of it), ScopeText the words.
// Orgs is who may use it (admin only; AllOrgs = every org). CanEdit: the
// viewer may change its scope and records, through ConnectorID, an org's
// (ConnectorOrg its slug, ConnectorOrgID its id) or the server's.
type ManagedDomain struct {
	Name           string          `json:"name"`
	Scope          string          `json:"scope,omitempty"`
	ScopeText      string          `json:"scope_text"`
	Via            string          `json:"via"`
	ConnectorID    string          `json:"connector_id"`
	ConnectorOrg   string          `json:"connector_org,omitempty"`
	ConnectorOrgID string          `json:"connector_org_id,omitempty"`
	AllOrgs        bool            `json:"all_orgs,omitempty"`
	Orgs           []string        `json:"orgs,omitempty"`
	CanEdit        bool            `json:"can_edit"`
	Records        []ManagedRecord `json:"records"`
}

// ManagedDomains is every managed domain and its records as orgID sees
// them (nil = an admin, who sees all). An org sees the domains it may use;
// on them its own tiles' records, and on its own connectors' domains the
// domain's and unused ones too. Panel and node addresses are the admin's.
func (o *Orchestrator) ManagedDomains(ctx context.Context, orgID *string) ([]ManagedDomain, error) {
	var zs []store.DNSDomain
	var err error
	if orgID == nil {
		zs, err = o.dns.All(ctx)
	} else {
		zs, err = o.dns.Usable(ctx, *orgID)
	}
	if err != nil {
		return nil, err
	}
	rs, err := o.dns.Records(ctx)
	if err != nil {
		return nil, err
	}
	hb, err := o.dns.HandedBack(ctx)
	if err != nil {
		return nil, err
	}
	byZone := map[string][]store.DNSRecord{}
	for _, r := range append(rs, hb...) {
		if r.DNSDomainID != nil {
			byZone[*r.DNSDomainID] = append(byZone[*r.DNSDomainID], r)
		}
	}
	panel := dnscap.Normal(o.panelDomain(ctx))
	tiles := map[string]tileOf{} // by tile domain id
	out := make([]ManagedDomain, 0, len(zs))
	for _, z := range zs {
		d, err := o.managedDomain(ctx, z, orgID)
		if err != nil {
			return nil, err
		}
		own := orgID == nil || z.OrgID != nil && *z.OrgID == *orgID
		for _, r := range byZone[z.ID] {
			m := ManagedRecord{
				Name: zoneHost(r.Name, r.Zone), Type: r.Type, Value: r.Value,
				PutBackAt: r.PutBackAt, HandedBackAt: r.HandedBackAt,
			}
			var t tileOf
			switch {
			case r.Kind == store.RecordName && m.Name == panel:
				m.Whose = WhosePanel
			case r.Kind == store.RecordName:
				m.Whose = WhoseNode
			case r.DomainID != nil:
				m.Whose = WhoseApp
				if t, err = o.tileOfDomain(ctx, tiles, *r.DomainID); err != nil {
					return nil, err
				}
				m.Host, m.Tile = t.host, t.usage
			case r.Kind == store.RecordCustom:
				m.Whose = WhoseDomain
			default:
				m.Whose = WhoseUnused
			}
			switch {
			case orgID == nil:
			case m.Whose == WhosePanel || m.Whose == WhoseNode:
				continue
			case m.Whose == WhoseApp:
				if m.Tile == nil || m.Tile.OrgID != *orgID {
					continue
				}
			case !own:
				continue
			}
			if r.HandedBackAt != nil {
				m.Whose = WhoseHandedBack
			} else if why := o.dns.NoMX(r.Zone); r.Kind == store.RecordCustom && r.Type == "MX" && why != "" {
				m.Drift = "Not live: " + strings.ToLower(why[:1]) + why[1:] // the sync skips it, the row stays
			} else if r.PutBackAt != nil {
				m.Drift = "Changed outside stackr; put back"
			}
			m.WhoseText = whoseWords[m.Whose]
			d.Records = append(d.Records, m)
		}
		slices.SortStableFunc(d.Records, func(a, b ManagedRecord) int {
			return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Type, b.Type), cmp.Compare(a.Value, b.Value))
		})
		out = append(out, d)
	}
	return out, nil
}

// ManagedDomainNamed is one of ManagedDomains by name; NotFound when the
// viewer sees no such domain.
func (o *Orchestrator) ManagedDomainNamed(ctx context.Context, orgID *string, name string) (ManagedDomain, error) {
	ds, err := o.ManagedDomains(ctx, orgID)
	if err != nil {
		return ManagedDomain{}, err
	}
	i := slices.IndexFunc(ds, func(d ManagedDomain) bool { return d.Name == dnscap.Normal(name) })
	if i < 0 {
		return ManagedDomain{}, errs.ErrNotFound
	}
	return ds[i], nil
}

// managedDomain is z's row of the page, records left out, as orgID sees it.
func (o *Orchestrator) managedDomain(ctx context.Context, z store.DNSDomain, orgID *string) (ManagedDomain, error) {
	d := ManagedDomain{
		Name: z.Name, Scope: z.Scope, ScopeText: "All of " + z.Name,
		Via: o.viaOf(ctx, z.ConnectorID), ConnectorID: z.ConnectorID,
		CanEdit: orgID == nil || z.OrgID != nil && *z.OrgID == *orgID,
		Records: []ManagedRecord{},
	}
	if z.Scope != "" {
		d.ScopeText = "Only under " + strings.TrimSuffix(z.Scope, "."+z.Name)
	}
	if z.OrgID != nil {
		og, err := o.orgs.Get(ctx, *z.OrgID)
		if err != nil {
			return d, err
		}
		d.ConnectorOrg, d.ConnectorOrgID = og.Slug, og.ID
	}
	if orgID != nil {
		return d, nil // never another org's name
	}
	switch {
	case z.OrgID != nil:
		d.Orgs = []string{d.ConnectorOrg}
	case z.ShareAll:
		d.AllOrgs = true
	default:
		for _, id := range z.OrgIDs {
			if og, err := o.orgs.Get(ctx, id); err == nil {
				d.Orgs = append(d.Orgs, og.Slug)
			}
		}
	}
	return d, nil
}

// tileOf is a tile domain's host and its tile; usage nil once the tile is
// gone.
type tileOf struct {
	host  string
	usage *Usage
}

// tileOfDomain is tile domain id's tileOf, read once per page.
func (o *Orchestrator) tileOfDomain(ctx context.Context, seen map[string]tileOf, id string) (tileOf, error) {
	if t, ok := seen[id]; ok {
		return t, nil
	}
	var t tileOf
	d, err := o.store.Domains.Get(ctx, id)
	switch {
	case err == nil:
		t.host = d.Host
		if u, err := o.usageOf(ctx, ScopeTile, d.TileID, ""); err == nil {
			t.usage = &u
		}
	case !errors.Is(err, errs.ErrNotFound):
		return t, err
	}
	seen[id] = t
	return t, nil
}
