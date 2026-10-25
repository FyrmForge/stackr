package service

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Moving the default ingress moves the panel and every route nobody picked an
// ingress for onto another node. DNS decides whether the internet follows, so
// the switch shows where each of those hosts points before it asks for the
// node's name.

// IngressHost is one host that would enter by the new default ingress.
type IngressHost struct {
	Host   string   `json:"host"`
	Panel  bool     `json:"panel"`  // the panel's own domain: it moves with the default
	Points []string `json:"points"` // the addresses it resolves to now
	OK     bool     `json:"ok"`     // it already points at the node
	Error  string   `json:"error"`  // the lookup failed
}

// IngressPlan is what a switch to Node changes. Hosts lists the panel domain
// first.
type IngressPlan struct {
	Node string `json:"node"`
	// Addrs are the node's public addresses. Known is false for a node whose
	// address is its NAT's, which stackr cannot read: no host is marked OK.
	Addrs []string      `json:"addrs"`
	Known bool          `json:"known"`
	Hosts []IngressHost `json:"hosts"`
}

// resolveHost looks a host up. Tests swap it.
var resolveHost = func(ctx context.Context, host string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupHost(ctx, host)
}

// ingressTarget is the node a switch would move to, or the reason it cannot:
// it is tagged ingress, online and reachable from the internet.
func (o *Orchestrator) ingressTarget(ctx context.Context, ref string) (store.Node, ClusterNode, error) {
	n, err := o.nodeByRef(ctx, ref)
	if err != nil {
		return n, ClusterNode{}, err
	}
	if n.Status == node.StatusPending {
		return n, ClusterNode{}, errs.ErrNotFound
	}
	if !slices.Contains(node.Tags(n), node.TagIngress) {
		return n, ClusterNode{}, errs.Invalidf("node", "%s is not tagged %s", n.Name, node.TagIngress)
	}
	if err := o.nodeUp(ctx, n.ID); err != nil {
		return n, ClusterNode{}, err
	}
	m, err := o.ClusterMap(ctx)
	if err != nil {
		return n, ClusterNode{}, err
	}
	for _, c := range m.Nodes {
		if c.ID == n.ID {
			return n, c, ingressReachable(m, c)
		}
	}
	return n, ClusterNode{}, errs.ErrNotFound
}

// ingressReachable refuses a node the internet cannot reach: no public
// address, and no port forward to it (the probe missed it, its network has
// no router of its own, or its router forwards to another node). An unknown
// forward (nobody answered, or the network is not on the map) is allowed.
func ingressReachable(m ClusterMap, c ClusterNode) error {
	if len(c.Public) > 0 {
		return nil
	}
	if c.NoForward {
		return errs.Conflictf("%s has no public IP and no port forward; the internet cannot reach it.", c.Name)
	}
	for _, net := range m.Networks {
		if net.ID != c.Net || net.ID == "unknown" { // no facts: nothing to tell
			continue
		}
		if !net.Router {
			return errs.Conflictf("%s has no public IP and its network has no router to forward to it; the internet cannot reach it.", c.Name)
		}
		if net.Forward != "" && net.Forward != c.ID {
			return errs.Conflictf("%s has no public IP and its router forwards to %s; the internet cannot reach it.", c.Name, nameIn(m, net.Forward))
		}
	}
	return nil
}

// nameIn is the map's name of node id, or the id.
func nameIn(m ClusterMap, id string) string {
	for _, n := range m.Nodes {
		if n.ID == id {
			return n.Name
		}
	}
	return id
}

// IngressSwitchPlan (server admin) lists the hosts that would enter by the
// node and where each points now.
func (o *Orchestrator) IngressSwitchPlan(ctx context.Context, by *Principal, ref string) (IngressPlan, error) {
	if err := nodeAdmin(by); err != nil {
		return IngressPlan{}, err
	}
	n, c, err := o.ingressTarget(ctx, ref)
	if err != nil {
		return IngressPlan{}, err
	}
	plan := IngressPlan{Node: n.Name, Addrs: append([]string{}, c.Public...), Known: len(c.Public) > 0, Hosts: []IngressHost{}}
	panel, others, err := o.defaultIngressHosts(ctx)
	if err != nil {
		return plan, err
	}
	if panel != "" {
		plan.Hosts = append(plan.Hosts, o.checkHost(ctx, panel, true, plan))
	}
	for _, h := range others {
		plan.Hosts = append(plan.Hosts, o.checkHost(ctx, h, false, plan))
	}
	return plan, nil
}

// defaultIngressHosts are the hosts that enter by the default ingress: the
// panel domain, every external route (they carry no ingress pick) and every
// tile domain whose tile has none. A wildcard is checked through a probe name.
func (o *Orchestrator) defaultIngressHosts(ctx context.Context) (panel string, others []string, err error) {
	panel, _ = o.settings.Get(ctx, "panel_domain")
	rs, err := o.routes.List(ctx)
	if err != nil {
		return "", nil, err
	}
	for _, r := range rs {
		others = append(others, r.Host)
	}
	ds, err := o.store.Domains.List(ctx)
	if err != nil {
		return "", nil, err
	}
	for _, d := range ds {
		pick, err := o.IngressFor(ctx, d.TileID)
		if err != nil {
			return "", nil, err
		}
		// a host in a managed domain needs no DNS change from anyone:
		// stackr repoints its records itself
		if pick == "" && !o.dnsFollows(ctx, d.TileID, d.Host) {
			others = append(others, d.Host)
		}
	}
	slices.Sort(others)
	return panel, slices.DeleteFunc(slices.Compact(others), func(h string) bool { return h == "" || h == panel }), nil
}

func (o *Orchestrator) checkHost(ctx context.Context, host string, panel bool, plan IngressPlan) IngressHost {
	h := IngressHost{Host: host, Panel: panel}
	name := host
	if rest, ok := strings.CutPrefix(host, "*."); ok {
		name = "stackr-probe." + rest
	}
	ips, err := resolveHost(ctx, name)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	slices.Sort(ips)
	h.Points = ips
	h.OK = plan.Known && len(ips) > 0 && !slices.ContainsFunc(ips, func(ip string) bool { return !slices.Contains(plan.Addrs, ip) })
	return h
}

// SetDefaultIngress (server admin) moves the default ingress to a node tagged
// ingress. confirm must be the node's name: the panel moves with it.
func (o *Orchestrator) SetDefaultIngress(ctx context.Context, by *Principal, ref, confirm string) (DefaultsOut, error) {
	if err := nodeAdmin(by); err != nil {
		return DefaultsOut{}, err
	}
	n, _, err := o.ingressTarget(ctx, ref)
	if err != nil {
		return DefaultsOut{}, err
	}
	if confirm != n.Name {
		return DefaultsOut{}, errs.Invalidf("confirm", "type %s to confirm", n.Name)
	}
	return o.setDefault(ctx, by, ref, node.TagIngress, settingDefaultIngress, auditDefaultIngress)
}

// ---- DNS of a move ----
//
// Every action that moves a host's ingress (an ingress pick, an env move
// through Runs on, a pin move) shows the same list: the records a
// hand-managed host needs, and the managed hosts a connector repoints.

// DNSRecord is one record a hand-managed host needs.
type DNSRecord struct {
	Type  string `json:"type"` // A or AAAA
	Name  string `json:"name"` // the host
	Value string `json:"value"`
}

// DNSHost is one host whose ingress moves. A hand-managed host has Records;
// a managed one has Connector, Old and New. Want is what it must resolve to;
// Points, OK and Error are the live check (CheckDNS fills them).
type DNSHost struct {
	Host      string      `json:"host"`
	Records   []DNSRecord `json:"records,omitempty"`
	Connector string      `json:"connector,omitempty"`
	Old       []string    `json:"old,omitempty"`
	New       []string    `json:"new,omitempty"`
	Want      []string    `json:"want"`
	Points    []string    `json:"points,omitempty"`
	OK        bool        `json:"ok"`
	Error     string      `json:"error,omitempty"`
}

// DNSMove is the hosts a move sends to another ingress node: Hand the ones
// the owner points, Managed the ones a connector repoints.
type DNSMove struct {
	Hand    []DNSHost `json:"hand"`
	Managed []DNSHost `json:"managed"`
}

// Empty says no host changes ingress.
func (m DNSMove) Empty() bool { return len(m.Hand) == 0 && len(m.Managed) == 0 }

// HandHosts are the hand-managed host names.
func (m DNSMove) HandHosts() []string {
	hs := make([]string, len(m.Hand))
	for i, h := range m.Hand {
		hs[i] = h.Host
	}
	return hs
}

// DNSForMove is the shared host list: for each tile in to (tile id -> the
// ingress node it will enter by, "" = the default ingress) whose ingress node
// changes, its domains split into hand-managed and managed hosts. Records
// come from nodeAddrs, so an AAAA row appears only for a node with a public
// IPv6. Other lanes (env banner, config plans) call it with the node each
// tile is heading for. It does not look hosts up; CheckDNS does.
func (o *Orchestrator) DNSForMove(ctx context.Context, to map[string]string) (DNSMove, error) {
	def, err := o.DefaultIngress(ctx)
	if err != nil {
		return DNSMove{}, err
	}
	or := func(n string) string {
		if n == "" {
			return def
		}
		return n
	}
	out := DNSMove{Hand: []DNSHost{}, Managed: []DNSHost{}}
	for tid, dest := range to {
		from, err := o.IngressFor(ctx, tid)
		if err != nil {
			return out, err
		}
		if or(from) == or(dest) {
			continue
		}
		ds, err := o.store.Domains.ListByTile(ctx, tid)
		if err != nil {
			return out, err
		}
		for _, d := range ds {
			h, managed, err := o.dnsHost(ctx, tid, d.Host, or(from), or(dest))
			if err != nil {
				return out, err
			}
			if managed {
				out.Managed = append(out.Managed, h)
			} else {
				out.Hand = append(out.Hand, h)
			}
		}
	}
	for _, l := range []*[]DNSHost{&out.Hand, &out.Managed} {
		slices.SortFunc(*l, func(a, b DNSHost) int { return strings.Compare(a.Host, b.Host) })
		*l = slices.CompactFunc(*l, func(a, b DNSHost) bool { return a.Host == b.Host })
	}
	return out, nil
}

// dnsHost is host's move from node from to node to.
func (o *Orchestrator) dnsHost(ctx context.Context, tileID, host, from, to string) (h DNSHost, managed bool, err error) {
	h = DNSHost{Host: host}
	v4, v6, _, err := o.nodeAddrs(ctx, to)
	if err != nil {
		return h, false, err
	}
	for _, a := range []netip.Addr{v4, v6} {
		if !a.IsValid() {
			continue
		}
		t := "A"
		if a.Is6() {
			t = "AAAA"
		}
		h.Records = append(h.Records, DNSRecord{Type: t, Name: host, Value: a.String()})
		h.Want = append(h.Want, a.String())
	}
	if !o.dnsFollows(ctx, tileID, host) {
		return h, false, nil
	}
	h.New, h.Records = h.Want, nil
	o4, o6, _, err := o.nodeAddrs(ctx, from)
	if err != nil {
		return h, true, err
	}
	for _, a := range []netip.Addr{o4, o6} {
		if a.IsValid() {
			h.Old = append(h.Old, a.String())
		}
	}
	h.Connector = host
	if t, err := o.tiles.Get(ctx, tileID); err == nil {
		if org, err := o.tileOrg(ctx, t); err == nil {
			if z, ok, _ := o.managedZone(ctx, org, host); ok {
				h.Connector = z.ConnectorID
				if c, err := o.conns.Of(ctx, z.OrgID, z.ConnectorID); err == nil {
					h.Connector = c.Name
				}
			}
		}
	}
	return h, true, nil
}

// CheckDNS looks every host up and marks the ones that resolve to Want.
// Hosts are looked up together, so a slow resolver costs one timeout.
func (o *Orchestrator) CheckDNS(ctx context.Context, m DNSMove) DNSMove {
	var wg sync.WaitGroup
	for _, l := range [][]DNSHost{m.Hand, m.Managed} {
		for i := range l {
			wg.Add(1)
			go func(h *DNSHost) {
				defer wg.Done()
				*h = checkResolves(ctx, *h)
			}(&l[i])
		}
	}
	wg.Wait()
	return m
}

// checkResolves is h with the live check filled in. A wildcard is looked up
// through a probe name.
func checkResolves(ctx context.Context, h DNSHost) DNSHost {
	name := h.Host
	if rest, ok := strings.CutPrefix(name, "*."); ok {
		name = "stackr-probe." + rest
	}
	h.Points, h.OK, h.Error = nil, false, ""
	ips, err := resolveHost(ctx, name)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	slices.Sort(ips)
	h.Points = ips
	h.OK = len(h.Want) > 0 && len(ips) > 0 && !slices.ContainsFunc(ips, func(ip string) bool { return !slices.Contains(h.Want, ip) })
	return h
}

// ---- keeping managed records ----

type keepDNSKey struct{}

// KeepDNS marks a move whose managed hosts keep their records: the connector
// does not repoint them (the untick in the move dialog).
func KeepDNS(ctx context.Context) context.Context { return context.WithValue(ctx, keepDNSKey{}, true) }

func keepsDNS(ctx context.Context) bool { v, _ := ctx.Value(keepDNSKey{}).(bool); return v }

// dnsHoldKey is the bookkeeping row of hosts whose records stackr leaves as
// they are, one per line.
const dnsHoldKey = "dns_hold"

// DNSHeld are the hosts whose managed records stay untouched until they match
// (dnsWants must hold them; the env banner lists them).
func (o *Orchestrator) DNSHeld(ctx context.Context) []string {
	v, _, _ := o.store.Settings.Get(ctx, dnsHoldKey)
	return strings.Fields(v)
}

// holdDNS adds hosts to the held set; ReleaseDNS drops them again.
func (o *Orchestrator) holdDNS(ctx context.Context, hosts []string) error {
	held := slices.Concat(o.DNSHeld(ctx), hosts)
	slices.Sort(held)
	return o.store.Settings.Set(ctx, dnsHoldKey, strings.Join(slices.Compact(held), "\n"))
}

// ReleaseDNS lets stackr repoint hosts again.
func (o *Orchestrator) ReleaseDNS(ctx context.Context, hosts []string) error {
	held := slices.DeleteFunc(o.DNSHeld(ctx), func(h string) bool { return slices.Contains(hosts, h) })
	return o.store.Settings.Set(ctx, dnsHoldKey, strings.Join(held, "\n"))
}

// keepManaged holds the move's managed hosts when the move asked to.
func (o *Orchestrator) keepManaged(ctx context.Context, m DNSMove) error {
	if !keepsDNS(ctx) || len(m.Managed) == 0 {
		return nil
	}
	hs := make([]string, len(m.Managed))
	for i, h := range m.Managed {
		hs[i] = h.Host
	}
	return o.holdDNS(ctx, hs)
}
