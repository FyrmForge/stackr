package service

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"slices"
	"strings"

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
	return o.conns.ListOf(ctx, orgID, CapDNS)
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
	return o.dns.Domains(ctx, c.ID, p)
}

// ManageDNSDomain makes stackr manage domain, or sets who may use a managed
// one: on a server connector the orgs in orgIDs, or every org when none.
func (o *Orchestrator) ManageDNSDomain(ctx context.Context, orgID *string, id, domain string, orgIDs []string) (DNSDomain, error) {
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
	if err != nil || !made {
		if err == nil {
			o.dnsChanged(ctx)
		}
		return d, err
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
	var users []string
	for _, d := range ds {
		u, err := o.domainUsers(ctx, d.Name)
		if err != nil {
			return err
		}
		users = append(users, u...)
	}
	if len(users) > 0 {
		return errs.Conflictf("The connector's domains are in use by %s. Remove those domains first", strings.Join(users, ", "))
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
