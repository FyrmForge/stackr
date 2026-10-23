package service

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
)

// dns.go holds the DNS connector verbs. orgID nil is the server's own
// connector (admin), else an org's (owner). The account key never leaves
// the service: rows come back with config stripped.

// DNS connector kinds and domain states.
const (
	DNSNamecheap  = dnscap.Namecheap
	DNSManaged    = dnscap.Managed
	DNSNotManaged = dnscap.NotManaged
	DNSElsewhere  = dnscap.Elsewhere
)

// DNSDomain is a domain on a DNS connector's account and its state.
type DNSDomain = dnscap.Domain

// DNSAccount is what the Namecheap form takes. ClientIP "" is stackr's own
// public IPv4 (DNSClientIP).
type DNSAccount struct {
	User     string `json:"user"`
	Key      string `json:"key"`
	ClientIP string `json:"client_ip"`
}

// DNSConnector is a just added connector and its account's domains.
type DNSConnector struct {
	Connector Connector   `json:"connector"`
	Domains   []DNSDomain `json:"domains"`
}

// DNSClientIP is the controller's public IPv4 as its node reports it, the
// address Namecheap sees the API calls come from; "" when it has none (a
// box behind NAT: the admin types the router's address).
func (o *Orchestrator) DNSClientIP(ctx context.Context) string {
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
	return ""
}

// AddDNSConnector checks the account (it must list its domains) and saves
// it as a DNS connector of kind.
func (o *Orchestrator) AddDNSConnector(ctx context.Context, orgID *string, kind string, a DNSAccount) (DNSConnector, error) {
	if a.ClientIP == "" {
		a.ClientIP = o.DNSClientIP(ctx)
	}
	creds := dnscap.Creds(a)
	zs, err := o.dns.Check(ctx, kind, creds)
	if err != nil {
		return DNSConnector{}, err
	}
	c, err := o.conns.AddDNS(ctx, orgID, kind, connector.DNS(creds))
	if err != nil {
		return DNSConnector{}, err
	}
	return DNSConnector{Connector: c, Domains: dnscap.States(zs)}, nil
}

// DNSConnectors is orgID's DNS connectors (nil = the server's).
func (o *Orchestrator) DNSConnectors(ctx context.Context, orgID *string) ([]Connector, error) {
	return o.conns.ListDNS(ctx, orgID)
}

// dnsAccount is connector id of orgID and its account.
func (o *Orchestrator) dnsAccount(ctx context.Context, orgID *string, id string) (Connector, dnscap.Creds, error) {
	c, err := o.conns.DNSConnector(ctx, orgID, id)
	if err != nil {
		return c, dnscap.Creds{}, err
	}
	d, ok := connector.DNSOf(c)
	if !ok {
		return c, dnscap.Creds{}, errs.ErrNotFound
	}
	c.Config = ""
	return c, dnscap.Creds(d), nil
}

// DNSDomains is every domain on connector id's account: managed, not
// managed, or DNS elsewhere.
func (o *Orchestrator) DNSDomains(ctx context.Context, orgID *string, id string) ([]DNSDomain, error) {
	c, creds, err := o.dnsAccount(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	return o.dns.Domains(ctx, c.ID, c.Provider, creds)
}

// ManageDNSDomain makes stackr manage domain, or sets who may use a managed
// one: on a server connector the orgs in orgIDs, or every org when none.
func (o *Orchestrator) ManageDNSDomain(ctx context.Context, orgID *string, id, domain string, orgIDs []string) (DNSDomain, error) {
	c, creds, err := o.dnsAccount(ctx, orgID, id)
	if err != nil {
		return DNSDomain{}, err
	}
	orgIDs = slices.Compact(slices.Sorted(slices.Values(orgIDs)))
	for _, oid := range orgIDs {
		if _, err := o.orgs.Get(ctx, oid); errors.Is(err, errs.ErrNotFound) {
			return DNSDomain{}, errs.Invalidf("org_ids", "no organization %q", oid)
		} else if err != nil {
			return DNSDomain{}, err
		}
	}
	d, err := o.dns.Manage(ctx, c.ID, c.OrgID, c.Provider, creds, domain, orgIDs)
	if err == nil {
		o.dnsChanged(ctx)
	}
	return d, err
}

// dnsChanged pushes the proxy config after the managed domains changed: its
// wildcard certificates follow them.
func (o *Orchestrator) dnsChanged(ctx context.Context) {
	if err := o.sync.Sync(ctx); err != nil {
		slog.Warn("proxy: push after a managed domain change", "err", err) // the periodic sync repairs it
	}
}

// UnmanageDNSDomain stops managing domain.
func (o *Orchestrator) UnmanageDNSDomain(ctx context.Context, orgID *string, id, domain string) error {
	c, _, err := o.dnsAccount(ctx, orgID, id)
	if err != nil {
		return err
	}
	if err := o.dns.Unmanage(ctx, c.ID, domain); err != nil {
		return err
	}
	o.dnsChanged(ctx)
	return nil
}

// TakeOverDNSDomain switches domain to the provider's own DNS; confirm is
// the domain typed out.
func (o *Orchestrator) TakeOverDNSDomain(ctx context.Context, orgID *string, id, domain, confirm string) error {
	c, creds, err := o.dnsAccount(ctx, orgID, id)
	if err != nil {
		return err
	}
	return o.dns.TakeOver(ctx, c.Provider, creds, domain, confirm)
}

// DeleteDNSConnector deletes a DNS connector and the domains it manages.
func (o *Orchestrator) DeleteDNSConnector(ctx context.Context, orgID *string, id string) error {
	if err := o.conns.DeleteDNS(ctx, orgID, id); err != nil {
		return err
	}
	o.dnsChanged(ctx)
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
