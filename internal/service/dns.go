package service

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"slices"
	"strings"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// dns.go holds the DNS capability's verbs, on any connection whose kind
// offers it. orgID nil is the server's own connection (admin), else an
// org's (owner). The account's secrets never leave the service: rows come
// back with settings stripped.

// Domain states.
const (
	DNSManaged    = dnscap.Managed
	DNSNotManaged = dnscap.NotManaged
	DNSElsewhere  = dnscap.Elsewhere
)

// DNSDomain is a domain on a DNS connector's account and its state.
type DNSDomain = dnscap.Domain

// DNSRef is one record in a zone: Name relative to it, "@" the apex.
type DNSRef = dnscap.Ref

// serverIPv4 is the controller's public IPv4, the address a DNS provider
// sees the API calls come from: its node's own, else behind NAT the
// router's as an outside worker sees it (the cluster map's Public). ""
// when neither is known (a single box behind NAT: the admin types the
// router's address).
func (o *Orchestrator) serverIPv4(ctx context.Context) string {
	if o.mesh == nil {
		return ""
	}
	id, err := o.controllerID(ctx)
	if err != nil {
		return ""
	}
	for _, a := range o.mesh.publicOf(id) {
		if a.Is4() {
			return a.String()
		}
	}
	m, err := o.ClusterMap(ctx)
	if err != nil {
		return ""
	}
	return natIPv4(m, id)
}

// natIPv4 is the public IPv4 nodes outside see node id's network's packets
// come from; "" when none has seen it.
func natIPv4(m ClusterMap, id string) string {
	i := slices.IndexFunc(m.Nodes, func(n ClusterNode) bool { return n.ID == id })
	if i < 0 || m.Nodes[i].Net == "" {
		return ""
	}
	for _, c := range m.Networks {
		if a, err := netip.ParseAddr(c.Public); c.ID == m.Nodes[i].Net && err == nil && a.Is4() {
			return a.String()
		}
	}
	return ""
}

// DNSConnectors is orgID's DNS connectors (nil = the server's).
func (o *Orchestrator) DNSConnectors(ctx context.Context, orgID *string) ([]Connector, error) {
	return o.conns.ListOf(ctx, orgID, CapDNS, false)
}

// DNSManaged is how many domains each of orgID's DNS connectors manages
// (nil = the server's), by connector id. No provider call.
func (o *Orchestrator) DNSManaged(ctx context.Context, orgID *string) (map[string]int, error) {
	cs, err := o.DNSConnectors(ctx, orgID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(cs))
	for _, c := range cs {
		if out[c.ID], err = o.dns.Managed(ctx, c.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// dnsAccount is DNS connector id of orgID, settings stripped, and its
// provider.
func (o *Orchestrator) dnsAccount(ctx context.Context, orgID *string, id string) (Connector, dnscap.Provider, error) {
	c, err := o.conns.Of(ctx, orgID, id)
	if err != nil {
		return c, nil, err
	}
	if !connector.Has(c, CapDNS) {
		return c, nil, errs.ErrNotFound
	}
	p, err := o.dnsOf(c)
	c.Settings = ""
	return c, p, err
}

// DNSDomains is every domain on connector id's account: managed, not
// managed, or DNS elsewhere.
func (o *Orchestrator) DNSDomains(ctx context.Context, orgID *string, id string) ([]DNSDomain, error) {
	c, p, err := o.dnsAccount(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	ds, err := o.dns.Domains(ctx, c.ID, p)
	o.failed(ctx, c.ID, err)
	return ds, err
}

// ManageDNSDomain makes stackr manage domain, or sets who may use a managed
// one: on a server connector the orgs in orgIDs, or every org when none.
func (o *Orchestrator) ManageDNSDomain(ctx context.Context, orgID *string, id, domain string, orgIDs []string) (DNSDomain, error) {
	return o.ManageDNSDomainAdopting(ctx, orgID, id, domain, orgIDs, nil)
}

// ManageDNSDomainAdopting is ManageDNSDomain that also adopts the records in
// adopt: ones PlanManageDNSDomain listed, already in the zone and pointing
// at our nodes. Anything else in adopt is ignored.
func (o *Orchestrator) ManageDNSDomainAdopting(ctx context.Context, orgID *string, id, domain string, orgIDs []string, adopt []DNSRef) (DNSDomain, error) {
	c, p, err := o.dnsAccount(ctx, orgID, id)
	if err != nil {
		return DNSDomain{}, err
	}
	orgIDs = slices.Compact(slices.Sorted(slices.Values(orgIDs)))
	for _, oid := range orgIDs {
		if _, err := o.orgs.Get(ctx, oid); errors.Is(err, errs.ErrNotFound) {
			return DNSDomain{}, errs.Invalidf("org_ids", "No organization %q", oid)
		} else if err != nil {
			return DNSDomain{}, err
		}
	}
	res, made, err := o.managedResource(ctx, c.OrgID, domain)
	if err != nil {
		return DNSDomain{}, err
	}
	d, err := o.dns.Manage(ctx, c.ID, c.OrgID, p, domain, orgIDs)
	if err != nil {
		return d, err
	}
	if len(adopt) > 0 { // before the sync: the records are stackr's by then
		plan, err := o.planManage(ctx, c, p, domain, orgIDs)
		if err != nil {
			return d, err
		}
		var take []DNSRef
		for _, a := range plan.Adopt {
			if slices.Contains(adopt, a.DNSRef) {
				take = append(take, a.DNSRef)
			}
		}
		if err := o.dns.Adopt(ctx, c.ID, d.Name, take); err != nil {
			return d, err
		}
	}
	if !made {
		o.dnsChanged(ctx)
		return d, nil
	}
	r, err := o.domainres.Adopt(ctx, res, ownerOf(DomainResource{OrgID: c.OrgID}), nil)
	if err != nil {
		return d, err
	}
	if err := o.dns.Linked(ctx, c.ID, domain, r.ID); err != nil {
		return d, err
	}
	o.dnsChanged(ctx)
	return d, nil
}

// dnsChanged pushes the proxy config after the managed domains changed: its
// wildcard certificates follow them.
func (o *Orchestrator) dnsChanged(ctx context.Context) {
	if err := o.sync.Sync(ctx); err != nil {
		slog.Warn("proxy: push after a managed domain change", "err", err) // the periodic sync repairs it
	}
}

// managedResource is the domain resource a managed domain stands for. The
// host may already be one at the connector's level (the server's root for a
// server connector, the org's own row for an org connector): that row is
// used as is. Anywhere else is refused. made: none yet, res is the row to
// make (instance level for a server connector, org level for an org's).
func (o *Orchestrator) managedResource(ctx context.Context, orgID *string, domain string) (res domainres.Spec, made bool, err error) {
	host := dnscap.Normal(domain)
	all, err := o.domainres.ListAll(ctx)
	if err != nil {
		return res, false, err
	}
	res = domainres.Spec{Level: domainres.Instance, Host: host}
	if orgID != nil {
		res = domainres.Spec{Level: domainres.Org, OwnerID: *orgID, Host: host}
	}
	i := slices.IndexFunc(all, func(r DomainResource) bool { return r.Host == host })
	if i < 0 {
		return res, true, nil
	}
	r := all[i]
	mine := r.Level == res.Level && (orgID == nil || r.OrgID != nil && *r.OrgID == *orgID)
	if !mine {
		return res, false, errs.Conflictf("%s is already a domain resource of another level or organization", host)
	}
	return res, false, nil
}

// UnmanageDNSDomain stops managing domain: refused while tiles use it
// (the error names them); else the records stackr made are deleted, then
// the domain is forgotten.
func (o *Orchestrator) UnmanageDNSDomain(ctx context.Context, orgID *string, id, domain string) error {
	c, p, err := o.dnsAccount(ctx, orgID, id)
	if err != nil {
		return err
	}
	if err := o.forgetDomain(ctx, c, p, domain); err != nil {
		return err
	}
	o.dnsChanged(ctx)
	return nil
}

// TakeOverDNSDomain switches domain to the provider's own DNS; confirm is
// the domain typed out.
func (o *Orchestrator) TakeOverDNSDomain(ctx context.Context, orgID *string, id, domain, confirm string) error {
	c, p, err := o.dnsAccount(ctx, orgID, id)
	if err != nil {
		return err
	}
	return o.dns.TakeOver(ctx, p, o.label(c.Kind), domain, confirm)
}

// forgetAll unmanages every domain DNS connector c manages, each as
// UnmanageDNSDomain does: what removing it does first. Refused while a tile
// uses any of them.
func (o *Orchestrator) forgetAll(ctx context.Context, c store.Connector) error {
	p, err := o.dnsOf(c)
	if err != nil {
		return err
	}
	ds, err := o.dns.ByConnector(ctx, c.ID)
	if err != nil {
		return err
	}
	var users []Usage
	for _, d := range ds {
		u, err := o.domainUsers(ctx, d.Name)
		if err != nil {
			return err
		}
		users = append(users, u...)
	}
	if len(users) > 0 {
		return &InUse{Msg: "The connector's domains are in use by", Tail: ". Remove those domains first", Usages: users}
	}
	for _, d := range ds {
		if err := o.forgetDomain(ctx, c, p, d.Name); err != nil {
			return err
		}
	}
	return nil
}

// UsableDNSDomains is the managed domains orgID may put tiles under: its
// own connectors', and the server connectors' shared with it.
func (o *Orchestrator) UsableDNSDomains(ctx context.Context, orgID string) ([]string, error) {
	rows, err := o.dns.Usable(ctx, orgID)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out, err
}

// DNSWrite is a record stackr will write when it manages a domain, for a
// host a tile already uses.
type DNSWrite struct {
	Host  string `json:"host"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// DNSAdopt is a record already in the zone that points at one of our
// nodes. Serves: a tile uses its host, so it starts ticked.
type DNSAdopt struct {
	DNSRef
	Host   string `json:"host"`
	Serves bool   `json:"serves"`
}

// DNSManagePlan is what managing a domain does: the records stackr writes
// and the existing ones it may adopt.
type DNSManagePlan struct {
	Writes []DNSWrite `json:"writes"`
	Adopt  []DNSAdopt `json:"adopt"`
}

// PlanManageDNSDomain is what ManageDNSDomain would write now and what it
// may adopt, for the confirm that comes first. orgIDs is who may use the
// domain, as ManageDNSDomain takes it. Wildcards and records pointing
// anywhere but our nodes are never listed.
func (o *Orchestrator) PlanManageDNSDomain(ctx context.Context, orgID *string, id, domain string, orgIDs []string) (DNSManagePlan, error) {
	c, p, err := o.dnsAccount(ctx, orgID, id)
	if err != nil {
		return DNSManagePlan{}, err
	}
	return o.planManage(ctx, c, p, domain, slices.Sorted(slices.Values(orgIDs)))
}

// nodeIPs is every address our nodes' records point at.
func (o *Orchestrator) nodeIPs(ctx context.Context) (map[string]bool, error) {
	ns, err := o.store.Nodes.List(ctx)
	if err != nil {
		return nil, err
	}
	ips := map[string]bool{}
	for _, n := range ns {
		v4, v6, _, err := o.nodeAddrs(ctx, n.ID)
		if err != nil {
			return nil, err
		}
		for _, a := range []netip.Addr{v4, v6} {
			if a.IsValid() {
				ips[a.String()] = true
			}
		}
	}
	return ips, nil
}

func (o *Orchestrator) planManage(ctx context.Context, c Connector, p dnscap.Provider, domain string, orgIDs []string) (DNSManagePlan, error) {
	zone := dnscap.Normal(domain)
	live, err := o.dns.Existing(ctx, p, zone)
	if err != nil {
		return DNSManagePlan{}, err
	}
	rows, err := o.dns.Records(ctx)
	if err != nil {
		return DNSManagePlan{}, err
	}
	ips, err := o.nodeIPs(ctx)
	if err != nil {
		return DNSManagePlan{}, err
	}
	// the zone as it will be managed: who may use it decides whose tiles count
	z := store.DNSDomain{Name: zone, OrgID: c.OrgID, ShareAll: c.OrgID == nil && len(orgIDs) == 0, OrgIDs: orgIDs}
	hosts, wants, err := o.zoneWants(ctx, z)
	if err != nil {
		return DNSManagePlan{}, err
	}
	plan := DNSManagePlan{Writes: []DNSWrite{}, Adopt: []DNSAdopt{}} // the API schema requires both arrays
	adoptable := dnscap.Adoptable(live, rows, zone, ips)
	for _, a := range adoptable {
		plan.Adopt = append(plan.Adopt, DNSAdopt{DNSRef: a, Host: zoneHost(a.Name, zone), Serves: hosts[strings.ToLower(a.Name)]})
	}
	for _, w := range wants {
		at := func(r libdns.RR) bool { return strings.EqualFold(r.Name, w.Name) }
		if slices.ContainsFunc(live, func(r libdns.RR) bool { return at(r) && r.Type == w.Type && r.Data == w.Value }) {
			continue // there already: an adoptable record, or stackr's own
		}
		// a record that blocks the name and is not ours to adopt is the
		// Replace question when the host is attached, not a write
		if slices.ContainsFunc(live, func(r libdns.RR) bool {
			return at(r) && !slices.ContainsFunc(adoptable, func(a dnscap.Ref) bool {
				return strings.EqualFold(a.Name, r.Name) && a.Type == r.Type && a.Value == r.Data
			})
		}) {
			continue
		}
		plan.Writes = append(plan.Writes, DNSWrite{Host: zoneHost(w.Name, zone), Type: w.Type, Value: w.Value})
	}
	return plan, nil
}

func zoneHost(name, zone string) string {
	if name == "@" {
		return zone
	}
	return name + "." + zone
}

// PlanAdoptDNSHost is the existing records host would meet in a managed
// domain orgID may use when a tile domain is added for it: ones pointing
// at our nodes, which stackr may adopt instead of refusing the host. None
// when host is in no managed domain, nothing there points at us, or a
// record pointing elsewhere is there too (that keeps the Replace question).
func (o *Orchestrator) PlanAdoptDNSHost(ctx context.Context, orgID, host string) ([]DNSAdopt, error) {
	z, ok, err := o.managedZone(ctx, orgID, host)
	if err != nil || !ok {
		return nil, err
	}
	a, err := o.dnsAccounts(ctx, z.ConnectorID, z.OrgID)
	if err != nil {
		return nil, err
	}
	live, err := o.dns.Existing(ctx, a, z.Name)
	if err != nil {
		return nil, err
	}
	rows, err := o.dns.Records(ctx)
	if err != nil {
		return nil, err
	}
	ips, err := o.nodeIPs(ctx)
	if err != nil {
		return nil, err
	}
	name := dnscap.Name(host, z.Name)
	var out []DNSAdopt
	for _, r := range dnscap.Adoptable(live, rows, z.Name, ips) {
		if strings.EqualFold(r.Name, name) {
			out = append(out, DNSAdopt{DNSRef: r, Host: dnscap.Normal(host), Serves: true})
		}
	}
	elsewhere := slices.ContainsFunc(live, func(r libdns.RR) bool {
		return strings.EqualFold(r.Name, name) &&
			!slices.ContainsFunc(out, func(a DNSAdopt) bool { return a.Type == r.Type && a.Value == r.Data }) &&
			!slices.ContainsFunc(rows, func(x store.DNSRecord) bool {
				return x.Zone == z.Name && strings.EqualFold(x.Name, r.Name) && x.Type == r.Type && x.Value == r.Data
			})
	})
	if elsewhere {
		return nil, nil
	}
	return out, nil
}

// AdoptDNSHost adopts the records PlanAdoptDNSHost lists for host: the
// answer to its one-row question. The caller attaches the domain after.
func (o *Orchestrator) AdoptDNSHost(ctx context.Context, orgID, host string) error {
	rs, err := o.PlanAdoptDNSHost(ctx, orgID, host)
	if err != nil || len(rs) == 0 {
		return err
	}
	z, _, err := o.managedZone(ctx, orgID, host)
	if err != nil {
		return err
	}
	refs := make([]DNSRef, len(rs))
	for i, r := range rs {
		refs[i] = r.DNSRef
	}
	return o.dns.Adopt(ctx, z.ConnectorID, z.Name, refs)
}
