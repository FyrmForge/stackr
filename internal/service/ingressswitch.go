package service

import (
	"context"
	"net"
	"slices"
	"strings"
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
	if err := o.nodeOnline(ctx, n); err != nil {
		return n, ClusterNode{}, err
	}
	m, err := o.ClusterMap(ctx)
	if err != nil {
		return n, ClusterNode{}, err
	}
	for _, c := range m.Nodes {
		if c.ID == n.ID {
			return n, c, ingressReachable(c)
		}
	}
	return n, ClusterNode{}, errs.ErrNotFound
}

// ingressReachable refuses a node the internet cannot reach: no public
// address and no port forward to it.
func ingressReachable(c ClusterNode) error {
	if len(c.Public) == 0 && c.NoForward {
		return errs.Conflictf("%s has no public IP and no port forward; the internet cannot reach it.", c.Name)
	}
	return nil
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
		if pick == "" {
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
