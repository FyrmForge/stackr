package service

import (
	"cmp"
	"context"
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// dnsrecords.go keeps the records of managed domains in step with the tile
// domains: an A (and an AAAA when the node has a public IPv6) for every
// host in a managed domain the tile's org may use, pointing at the node
// whose proxy serves it. Every proxy push kicks reconcileDNS (syncDNS), so a domain
// added, an ingress moved and a domain removed all land the same way.

// RecordTaken refuses a host whose name already has a DNS record stackr
// did not create; attaching again with replace takes the record over.
type RecordTaken = dnscap.RecordTaken

// NoPublicIP refuses records for Hosts: their ingress node has no public
// address, detected or set. The panel links the node's Network tab.
type NoPublicIP struct {
	NodeID, Node string
	Hosts        []string
}

func (e *NoPublicIP) Error() string {
	return "Node " + e.Node + " has no public IP address for DNS records; set one on the node"
}

// Is makes errors.Is(err, errs.ErrRefused) true.
func (e *NoPublicIP) Is(target error) bool { return target == errs.ErrRefused }

// ManagedHost is the host a tile domain gets from a managed domain and a
// subdomain ("" = the domain itself).
func ManagedHost(domain, sub string) string {
	domain, sub = dnscap.Normal(domain), strings.Trim(strings.ToLower(strings.TrimSpace(sub)), ".")
	if sub == "" {
		return domain
	}
	return sub + "." + domain
}

// dnsAccounts opens a managed domain's connector account.
func (o *Orchestrator) dnsAccounts(ctx context.Context, connectorID string, orgID *string) (dnscap.Provider, error) {
	_, p, err := o.dnsAccount(ctx, orgID, connectorID)
	return p, err
}

// ipv6On is the admin's IPv6 setting, off until set.
func (o *Orchestrator) ipv6On(ctx context.Context) bool {
	v, _ := o.settings.Get(ctx, "ipv6")
	b, _ := strconv.ParseBool(v)
	return b
}

// nodeAddrs is where a node's records point: its public IP override, else
// the public IPv4 and IPv6 it reports. Invalid ones stay unset, and v6 stays
// unset while the IPv6 setting is off.
func (o *Orchestrator) nodeAddrs(ctx context.Context, id string) (v4, v6 netip.Addr, name string, err error) {
	n, err := o.store.Nodes.Get(ctx, id)
	if err != nil {
		return v4, v6, "", err
	}
	var as []netip.Addr
	if a, err := netip.ParseAddr(n.PublicIP); err == nil {
		as = []netip.Addr{a}
	} else if n.Role == node.Controller {
		if f := o.controllerFacts(ctx, n); f != nil {
			as = publicAddrs(*f)
		}
	} else if o.mesh != nil {
		as = o.mesh.publicOf(n.ID)
	}
	v6ok := o.ipv6On(ctx)
	for _, a := range as {
		switch {
		case a.Is4() && !v4.IsValid():
			v4 = a
		case v6ok && a.Is6() && !a.Is4In6() && !v6.IsValid():
			v6 = a
		}
	}
	return v4, v6, n.Name, nil
}

// tileOrg is the org a tile belongs to.
func (o *Orchestrator) tileOrg(ctx context.Context, t store.Tile) (string, error) {
	st, err := o.stacks.Get(ctx, t.StackID)
	return st.OrgID, err
}

// dnsWants is every record stackr should have now. A node with no public
// address yields an error naming it and a hold on its hosts' records (left
// as they are); the other records still come back.
func (o *Orchestrator) dnsWants(ctx context.Context) ([]dnscap.Want, error) {
	zones, err := o.dns.All(ctx)
	if err != nil || len(zones) == 0 {
		return nil, err
	}
	ds, err := o.domains.List(ctx)
	if err != nil {
		return nil, err
	}
	def, err := o.DefaultIngress(ctx)
	if err != nil {
		return nil, err
	}
	type tileFacts struct {
		org, node string
		err       error
	}
	tiles := map[string]tileFacts{}
	var out []dnscap.Want
	var bad []error
	noIP := map[string]*NoPublicIP{}
	held := o.DNSHeld(ctx)
	for _, d := range ds {
		if strings.HasPrefix(d.Host, "*.") {
			continue
		}
		z, ok := dnscap.ZoneOf(d.Host, zones)
		if !ok {
			continue
		}
		if slices.Contains(held, d.Host) { // unticked in a move: the record stays as it is
			out = append(out, dnscap.Want{Zone: z.Name, Name: dnscap.Name(d.Host, z.Name), Hold: true})
			continue
		}
		f, seen := tiles[d.TileID]
		if !seen {
			t, err := o.tiles.Get(ctx, d.TileID)
			if err == nil {
				f.org, f.err = o.tileOrg(ctx, t)
			} else {
				f.err = err
			}
			if f.err == nil {
				f.node, f.err = o.IngressFor(ctx, t.ID)
				if f.node == "" {
					f.node = def
				}
			}
			tiles[d.TileID] = f
		}
		if f.err != nil {
			return nil, f.err
		}
		if !dnscap.MayUse(z, f.org) {
			continue // an auto name under a domain its org may not use: the admin's own DNS
		}
		v4, v6, name, err := o.nodeAddrs(ctx, f.node)
		if err != nil {
			return nil, err
		}
		if !v4.IsValid() && !v6.IsValid() {
			if noIP[f.node] == nil {
				noIP[f.node] = &NoPublicIP{NodeID: f.node, Node: name}
				bad = append(bad, noIP[f.node])
			}
			noIP[f.node].Hosts = append(noIP[f.node].Hosts, d.Host)
			out = append(out, dnscap.Want{Zone: z.Name, Name: dnscap.Name(d.Host, z.Name), Hold: true})
			continue
		}
		w := dnscap.Want{DNSDomainID: z.ID, Zone: z.Name, Name: dnscap.Name(d.Host, z.Name), DomainID: d.ID}
		if v4.IsValid() {
			w.Type, w.Value = "A", v4.String()
			out = append(out, w)
		}
		if v6.IsValid() {
			w.Type, w.Value = "AAAA", v6.String()
			out = append(out, w)
		}
	}
	return out, errors.Join(bad...)
}

// reconcileDNS brings the managed domains' records to the tile domains.
// replace names the hosts whose foreign records may be taken over.
func (o *Orchestrator) reconcileDNS(ctx context.Context, replace map[string]bool) error {
	if err := o.releaseMatchedHolds(ctx); err != nil {
		return err
	}
	wants, werr := o.dnsWants(ctx)
	if wants == nil && werr != nil {
		return werr // the list itself failed: nothing to go on
	}
	return errors.Join(werr, o.dns.Reconcile(ctx, wants, replace, o.dnsAccounts))
}

// managedZone is the managed domain host sits in, for a tile of orgID. ok
// false: none. A domain the org may not use refuses a literal host.
func (o *Orchestrator) managedZone(ctx context.Context, orgID, host string) (store.DNSDomain, bool, error) {
	zones, err := o.dns.All(ctx)
	if err != nil {
		return store.DNSDomain{}, false, err
	}
	z, ok := dnscap.ZoneOf(host, zones)
	if !ok {
		return z, false, nil
	}
	if !dnscap.MayUse(z, orgID) {
		return z, false, errs.Conflictf("%s is managed by stackr for another organization; pick one of yours", z.Name)
	}
	return z, true, nil
}

// checkRecordFree refuses host when its name in z has a record stackr did
// not make, unless replace.
func (o *Orchestrator) checkRecordFree(ctx context.Context, z store.DNSDomain, host string, replace bool) error {
	if replace {
		return nil
	}
	a, err := o.dnsAccounts(ctx, z.ConnectorID, z.OrgID)
	if err != nil {
		return err
	}
	taken, err := o.dns.Taken(ctx, a, z.Name, host)
	if err != nil {
		return err
	}
	if taken {
		return &RecordTaken{Hosts: []string{host}}
	}
	return nil
}

// TileDNSDomains is the managed domains a tile's domain may sit in: the
// tile domain form's list.
func (o *Orchestrator) TileDNSDomains(ctx context.Context, tileID string) ([]string, error) {
	t, err := o.tiles.Get(ctx, tileID)
	if err != nil {
		return nil, err
	}
	org, err := o.tileOrg(ctx, t)
	if err != nil {
		return nil, err
	}
	return o.UsableDNSDomains(ctx, org)
}

// domainUsers is the tile domains that sit in zone, for the refusal that
// keeps a domain in use managed.
func (o *Orchestrator) domainUsers(ctx context.Context, zone string) ([]Usage, error) {
	ds, err := o.domains.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Usage
	for _, d := range ds {
		if dnscap.Name(d.Host, zone) == "" {
			continue
		}
		u, err := o.usageOf(ctx, ScopeTile, d.TileID, d.Host)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b Usage) int { return strings.Compare(a.Path+a.What, b.Path+b.What) })
	return out, nil
}

// forgetDomain deletes the records stackr made in a managed domain, the
// domain row, and the domain resource stackr made for it. Refused while a
// tile uses the domain.
func (o *Orchestrator) forgetDomain(ctx context.Context, c Connector, p dnscap.Provider, name string) error {
	users, err := o.domainUsers(ctx, dnscap.Normal(name))
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return &InUse{Msg: dnscap.Normal(name) + " is in use by", Tail: "; remove those domains first", Usages: users}
	}
	if err := o.dns.Forget(ctx, p, dnscap.Normal(name)); err != nil {
		return err
	}
	row, err := o.dns.Unmanage(ctx, c.ID, name)
	if err != nil || row.ResourceID == nil {
		return err
	}
	r, err := o.domainres.Get(ctx, *row.ResourceID)
	if errors.Is(err, errs.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	return o.domainres.Delete(ctx, r, 0)
}

// checkNotManaged refuses changing a domain resource whose host is a
// managed domain: the DNS connector holds it.
func (o *Orchestrator) checkNotManaged(ctx context.Context, r DomainResource) error {
	zones, err := o.dns.All(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(zones, func(z store.DNSDomain) bool { return z.Name == r.Host }) {
		return errs.Conflictf("%s is managed through a DNS connector; unmanage it there first", r.Host)
	}
	return nil
}

// SetNodePublicIP (server admin) sets the address a node's DNS records
// point at instead of the detected one (a floating IP, a load balancer);
// "" goes back to detected. The records follow at once.
func (o *Orchestrator) SetNodePublicIP(ctx context.Context, by *Principal, ref, ip string) (NodeOut, error) {
	if err := nodeAdmin(by); err != nil {
		return NodeOut{}, err
	}
	n, err := o.nodeByRef(ctx, ref)
	if err != nil {
		return NodeOut{}, err
	}
	if n.Status == node.StatusPending {
		return NodeOut{}, errs.ErrNotFound
	}
	ip = strings.TrimSpace(ip)
	if ip != "" {
		a, err := netip.ParseAddr(ip)
		if err != nil || !a.IsGlobalUnicast() || a.IsPrivate() || cgnat.Contains(a) {
			return NodeOut{}, errs.Invalidf("public_ip", "give a public IPv4 or IPv6 address")
		}
		ip = a.Unmap().String()
	}
	n.PublicIP = ip
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.Nodes.Update(ctx, n); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: auditNodeIP, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: "node", TargetID: n.ID, Detail: cmp.Or(ip, "detected"),
		})
	})
	if err != nil {
		return NodeOut{}, err
	}
	out, err := o.nodeOut(ctx, n)
	if err != nil {
		return out, err
	}
	return out, o.reconcileDNS(ctx, nil) // saved either way; a provider error is the caller's to see

}

// IngressDomain is a tile domain an ingress node serves: Org, Stack and
// Env are slugs, for the "Move ingress" link to the env's Placement tab.
type IngressDomain struct {
	Host, Org, Stack, Env string
}

// servedBy is every tile domain whose ingress node is nodeID ("" = any),
// with that node.
func (o *Orchestrator) servedBy(ctx context.Context, nodeID string, envID string) ([]IngressDomain, []string, error) {
	ds, err := o.domains.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	def, err := o.DefaultIngress(ctx)
	if err != nil {
		return nil, nil, err
	}
	var out []IngressDomain
	var nodes []string
	for _, d := range ds {
		t, err := o.tiles.Get(ctx, d.TileID)
		if err != nil {
			return nil, nil, err
		}
		if envID != "" && t.EnvironmentID != envID {
			continue
		}
		in, err := o.IngressFor(ctx, t.ID)
		if err != nil {
			return nil, nil, err
		}
		in = cmp.Or(in, def)
		if nodeID != "" && in != nodeID {
			continue
		}
		ch, og, err := o.chain(ctx, ScopeTile, t.ID)
		if err != nil {
			return nil, nil, err
		}
		st, err1 := o.store.Stacks.Get(ctx, ch[2].id)
		e, err2 := o.store.Environments.Get(ctx, ch[1].id)
		if err := errors.Join(err1, err2); err != nil {
			return nil, nil, err
		}
		out = append(out, IngressDomain{Host: d.Host, Org: og.Slug, Stack: st.Slug, Env: e.Slug})
		nodes = append(nodes, in)
	}
	return out, nodes, nil
}

// DownIngressDomains is the domains node id serves while it is down (none
// while it is up): the node page lists them, each with Move ingress.
func (o *Orchestrator) DownIngressDomains(ctx context.Context, id string) ([]IngressDomain, error) {
	if o.nodeUp(ctx, id) == nil {
		return nil, nil
	}
	ds, _, err := o.servedBy(ctx, id, "")
	return ds, err
}

// IngressDown is an env's domains whose ingress node is down.
type IngressDown struct {
	Node  string
	Hosts []string
}

// EnvIngressDown reports the env's domains served by a down ingress node.
func (o *Orchestrator) EnvIngressDown(ctx context.Context, envID string) (IngressDown, bool) {
	ds, nodes, err := o.servedBy(ctx, "", envID)
	if err != nil {
		return IngressDown{}, false
	}
	var out IngressDown
	for i, d := range ds {
		var down *NodeDownError
		if errors.As(o.nodeUp(ctx, nodes[i]), &down) {
			out.Node = down.Name
			out.Hosts = append(out.Hosts, d.Host)
		}
	}
	slices.Sort(out.Hosts)
	out.Hosts = slices.Compact(out.Hosts)
	return out, len(out.Hosts) > 0
}

// dnsFollows says whether stackr points host's records itself: a host in
// a managed domain the tile's org may use. An ingress move needs no DNS
// change from anyone for it.
func (o *Orchestrator) dnsFollows(ctx context.Context, tileID, host string) bool {
	t, err := o.tiles.Get(ctx, tileID)
	if err != nil {
		return false
	}
	org, err := o.tileOrg(ctx, t)
	if err != nil {
		return false
	}
	_, ok, err := o.managedZone(ctx, org, host)
	return ok && err == nil && !strings.HasPrefix(host, "*.")
}

// zoneWants is the records tile domains under zone z would get: one A, and
// an AAAA when the node has a public IPv6, per host. hosts is the names
// (lower case, relative) a tile serves. A node with no public address
// yields nothing.
func (o *Orchestrator) zoneWants(ctx context.Context, z store.DNSDomain) (hosts map[string]bool, wants []dnscap.Want, err error) {
	ds, err := o.domains.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	def, err := o.DefaultIngress(ctx)
	if err != nil {
		return nil, nil, err
	}
	hosts = map[string]bool{}
	for _, d := range ds {
		name := dnscap.Name(d.Host, z.Name)
		if name == "" || strings.HasPrefix(d.Host, "*.") {
			continue
		}
		t, err := o.tiles.Get(ctx, d.TileID)
		if err != nil {
			return nil, nil, err
		}
		org, err := o.tileOrg(ctx, t)
		if err != nil {
			return nil, nil, err
		}
		if !dnscap.MayUse(z, org) {
			continue
		}
		in, err := o.IngressFor(ctx, t.ID)
		if err != nil {
			return nil, nil, err
		}
		v4, v6, _, err := o.nodeAddrs(ctx, cmp.Or(in, def))
		if err != nil {
			return nil, nil, err
		}
		hosts[strings.ToLower(name)] = true
		if v4.IsValid() {
			wants = append(wants, dnscap.Want{Name: name, Type: "A", Value: v4.String()})
		}
		if v6.IsValid() {
			wants = append(wants, dnscap.Want{Name: name, Type: "AAAA", Value: v6.String()})
		}
	}
	return hosts, wants, nil
}
