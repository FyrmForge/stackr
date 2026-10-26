package service

import (
	"context"
	"log/slog"
	"strings"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// nodeaddress.go is a node's own DNS name (its Address): free text, any
// scheme. stackr writes its record when the name sits in a managed scope
// (nameWants); anywhere else the drawer shows the record to set by hand.

const auditNodeAddress = "node.address"

// checkNodeAddress is raw as a node's Address stores it: "" for none, else
// one host name that no other node, the panel or a tile domain holds. self
// is the node's own id ("" for one not made yet).
func (o *Orchestrator) checkNodeAddress(ctx context.Context, self, raw string) (string, error) {
	if raw = strings.TrimSpace(raw); raw == "" {
		return "", nil
	}
	host, err := installspec.CheckRoot(raw)
	if err != nil {
		return "", errs.Invalidf("node_address", "%s.", strings.TrimSuffix(err.Error(), "."))
	}
	if strings.HasPrefix(host, "*.") {
		return "", errs.Invalidf("node_address", "Give one name, not a wildcard.")
	}
	if host == o.panelDomain(ctx) {
		return "", errs.Invalidf("node_address", "%s is the panel's address.", host)
	}
	ns, err := o.store.Nodes.List(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range ns {
		if n.ID != self && n.Address == host {
			return "", errs.Invalidf("node_address", "Node %s already has %s.", n.Name, host)
		}
	}
	ds, err := o.domains.List(ctx)
	if err != nil {
		return "", err
	}
	for _, d := range ds {
		if d.Host == host {
			return "", errs.Invalidf("node_address", "%s is a tile's domain.", host)
		}
	}
	return host, nil
}

// SetNodeAddress (server admin) sets a node's own DNS name; "" clears it.
// Its record follows at once when the name sits in a managed scope.
func (o *Orchestrator) SetNodeAddress(ctx context.Context, by *Principal, ref, addr string) (NodeOut, error) {
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
	if n.Address, err = o.checkNodeAddress(ctx, n.ID, addr); err != nil {
		return NodeOut{}, err
	}
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.Nodes.Update(ctx, n); err != nil {
			return err
		}
		detail := n.Address
		if detail == "" {
			detail = "none"
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: auditNodeAddress, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: "node", TargetID: n.ID, Detail: detail,
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

// NodeDNS is a node's own DNS record as its drawer shows it. Managed: the
// name sits in a managed scope and stackr writes Records itself; else
// Records is what to set by hand. None of them when the node has no public
// address (NoPublic) or no Address.
type NodeDNS struct {
	Address  string
	Managed  bool
	NoPublic bool
	Records  []DNSWrite
}

// NodeDNSOf is node id's NodeDNS.
func (o *Orchestrator) NodeDNSOf(ctx context.Context, id string) (NodeDNS, error) {
	n, err := o.store.Nodes.Get(ctx, id)
	if err != nil || n.Address == "" {
		return NodeDNS{}, err
	}
	zones, err := o.dns.All(ctx)
	if err != nil {
		return NodeDNS{}, err
	}
	_, managed := dnscap.ZoneOf(n.Address, zones)
	out := NodeDNS{Address: n.Address, Managed: managed}
	v4, v6, _, err := o.nodeAddrs(ctx, id)
	if err != nil {
		return out, err
	}
	for _, w := range addrWants(dnscap.Want{}, v4, v6) {
		out.Records = append(out.Records, DNSWrite{Host: n.Address, Type: w.Type, Value: w.Value})
	}
	out.NoPublic = len(out.Records) == 0
	return out, nil
}

// defaultControllerAddress gives the controller n1.<panel address> while
// it has no Address and a panel address is set: the first node's name.
func (o *Orchestrator) defaultControllerAddress(ctx context.Context) {
	p := o.panelDomain(ctx)
	if p == "" {
		return
	}
	n, err := o.nodeLeaf().Controller(ctx)
	if err != nil || n.Address != "" {
		return
	}
	addr, err := o.checkNodeAddress(ctx, n.ID, "n1."+p)
	if err != nil {
		return // taken by something else: the admin picks one in the drawer
	}
	n.Address = addr
	if err := o.store.Nodes.Update(ctx, n); err != nil {
		slog.Warn("nodes: controller address", "err", err)
	}
}

// NodeAddressSuffix is what a new node's Address defaults to after its
// name: ".<panel address>", "" while no panel address is set (no default).
func (o *Orchestrator) NodeAddressSuffix(ctx context.Context) string {
	if p := o.panelDomain(ctx); p != "" {
		return "." + p
	}
	return ""
}
