package service

import (
	"context"
	"log/slog"
	"net/netip"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/vip"
)

// Cross-node links: a consumer on another node than its managed instance
// reaches the instance's VIP over the mesh, by the same alias it would use on
// the instance network (an ExtraHosts line), through one flow per consumer
// env and instance port.

// tileVIP is tile tileID's VIP on node: its pause container's address on the
// env network, as stored (tile_vips). A tile not stored yet (deployed before
// VIPs were) is read from Docker once and stored.
func (o *Orchestrator) tileVIP(ctx context.Context, node, tileID string) (netip.Addr, bool) {
	s, _ := o.store.TileVIPs.Get(ctx, tileID, node)
	if s == "" {
		cs, err := o.docker.List(WithNode(ctx, node), map[string]string{"stackr.tile": tileID, "stackr.role": "pause"})
		if err != nil || len(cs) == 0 || len(cs[0].IPs) == 0 {
			return netip.Addr{}, false
		}
		s = cs[0].IPs[0]
		_ = o.store.TileVIPs.Set(ctx, tileID, node, s)
	}
	v, err := netip.ParseAddr(s)
	return v, err == nil
}

// reconcileVIP stores real, tile t's pause address on node read off the
// rebuild's list, when the stored one differs. A stored one that changed is
// drift (a pause recreated by hand, an old unpinned pause restarted): logged.
func (o *Orchestrator) reconcileVIP(ctx context.Context, node string, t store.Tile, real string) {
	if real == "" {
		return
	}
	stored, err := o.store.TileVIPs.Get(ctx, t.ID, node)
	if err != nil || stored == real {
		return
	}
	if stored != "" {
		slog.Warn("vip: address drift, stored the real one", "tile", t.Slug, "node", node, "stored", stored, "real", real)
	}
	if err := o.store.TileVIPs.Set(ctx, t.ID, node, real); err != nil {
		slog.Warn("vip: store", "tile", t.Slug, "err", err)
	}
}

// pinVIP is tile.Leaf.Pin: the stored VIP when the tile had one on this node
// (a recreated pause keeps its address), else a stable pick in the env
// network's lower half, clear of every address in use there.
func (o *Orchestrator) pinVIP(ctx context.Context, t store.Tile, network string) string {
	node, err := o.envNode(ctx, t.EnvironmentID)
	if err != nil {
		return ""
	}
	if v, _ := o.store.TileVIPs.Get(ctx, t.ID, node); v != "" {
		return v
	}
	e, err := o.store.Environments.Get(ctx, t.EnvironmentID)
	if err != nil {
		return ""
	}
	sub, err := netip.ParsePrefix(e.Subnet)
	if err != nil {
		return ""
	}
	rows, err := o.docker.List(WithNode(ctx, node), map[string]string{docker.LabelManaged: "true"})
	if err != nil {
		return ""
	}
	used := map[netip.Addr]bool{}
	for _, c := range rows {
		if a, err := netip.ParseAddr(c.Nets[network]); err == nil {
			used[a] = true
		}
	}
	return docker.StaticAddr(sub, t.ID, func(a netip.Addr) bool { return used[a] })
}

// linkHost is deploy.Flow.LinkHost: "alias:vip" when the instance runs on
// another node than env e, "" when they share one.
func (o *Orchestrator) linkHost(ctx context.Context, e store.Environment, instanceID string) (string, error) {
	m, err := o.store.ManagedInstances.Get(ctx, instanceID)
	if err != nil {
		return "", err
	}
	t, err := o.store.Tiles.Get(ctx, m.TileID)
	if err != nil {
		return "", err
	}
	in, err := o.envNode(ctx, t.EnvironmentID)
	if err != nil {
		return "", err
	}
	cn, err := o.envNode(ctx, e.ID)
	if err != nil || in == cn {
		return "", err
	}
	v, ok := o.tileVIP(ctx, in, t.ID)
	if !ok {
		return "", errs.Conflictf("%s runs on another node and has no address there yet: deploy it first", t.Slug)
	}
	return managed.Alias(t.Slug, m.ID) + ":" + v.String(), nil
}

// link is one binding seen across nodes.
type link struct {
	consNode, instNode string
	src                netip.Prefix // the consumer env's subnet
	vip                netip.Addr
	port               int
}

func (o *Orchestrator) linkOf(ctx context.Context, b store.Binding) (link, bool) {
	var l link
	p, err := o.store.Provisions.Get(ctx, b.ProvisionID)
	if err != nil {
		return l, false
	}
	m, err := o.store.ManagedInstances.Get(ctx, p.InstanceID)
	if err != nil {
		return l, false
	}
	eng, ok := mflow.Engines[m.Engine]
	if !ok {
		return l, false
	}
	it, err := o.store.Tiles.Get(ctx, m.TileID)
	if err != nil {
		return l, false
	}
	ct, err := o.store.Tiles.Get(ctx, b.ConsumerTileID)
	if err != nil {
		return l, false
	}
	ce, err := o.store.Environments.Get(ctx, ct.EnvironmentID)
	if err != nil {
		return l, false
	}
	if l.src, err = netip.ParsePrefix(ce.Subnet); err != nil {
		return l, false
	}
	if l.instNode, err = o.envNode(ctx, it.EnvironmentID); err != nil {
		return l, false
	}
	if l.consNode, err = o.envNode(ctx, ce.ID); err != nil || l.consNode == l.instNode {
		return l, false
	}
	if l.vip, ok = o.tileVIP(ctx, l.instNode, it.ID); !ok {
		return l, false
	}
	l.port = eng.Definition().Port
	return l, true
}

// linkFlows are node n's flows for cross-node links: Out for its consumers of
// instances elsewhere, an accept for consumers elsewhere of its instances.
func (o *Orchestrator) linkFlows(ctx context.Context, n store.Node) []vip.Flow {
	envs, err := o.store.Environments.ListByNode(ctx, n.ID)
	if err != nil {
		return nil
	}
	seen := map[vip.Flow]bool{}
	var out []vip.Flow
	add := func(b store.Binding, outward bool) {
		l, ok := o.linkOf(ctx, b)
		if !ok || outward != (l.consNode == n.ID) {
			return
		}
		f := vip.Flow{Src: l.src, VIP: l.vip, Port: l.port, Out: outward}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	for _, e := range envs {
		ts, err := o.store.Tiles.ListByEnv(ctx, e.ID)
		if err != nil {
			continue
		}
		for _, t := range ts {
			bs, _ := o.store.Bindings.ListByConsumer(ctx, t.ID)
			for _, b := range bs {
				add(b, true)
			}
			for _, b := range o.instanceBindings(ctx, t.ID) {
				add(b, false)
			}
		}
	}
	return out
}

// instanceBindings are every binding on the slices of the instance tile
// tileID; none when it is not a managed instance.
func (o *Orchestrator) instanceBindings(ctx context.Context, tileID string) []store.Binding {
	m, err := o.store.ManagedInstances.GetByTile(ctx, tileID)
	if err != nil {
		return nil
	}
	ps, err := o.store.Provisions.ListByInstance(ctx, m.ID)
	if err != nil {
		return nil
	}
	var out []store.Binding
	for _, p := range ps {
		bs, _ := o.store.Bindings.ListByProvision(ctx, p.ID)
		out = append(out, bs...)
	}
	return out
}

// relinkMoved redeploys the running consumers outside env envID of the
// instances it holds: a move changed the instance's VIP, or put it on or off
// the consumer's node. Consumers inside the env moved with it.
func (o *Orchestrator) relinkMoved(ctx context.Context, envID string) error {
	ts, err := o.store.Tiles.ListByEnv(ctx, envID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var cons []Tile
	for _, t := range ts {
		for _, b := range o.instanceBindings(ctx, t.ID) {
			if seen[b.ConsumerTileID] {
				continue
			}
			seen[b.ConsumerTileID] = true
			ct, err := o.store.Tiles.Get(ctx, b.ConsumerTileID)
			if err == nil && ct.EnvironmentID != envID {
				cons = append(cons, ct)
			}
		}
	}
	return o.redeployRunning(ctx, cons)
}
