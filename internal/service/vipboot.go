package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/cluster"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/vip"
	"github.com/FyrmForge/stackr/internal/worker"
)

// rebuildVIPs re-declares the VIP rule table from Docker: at boot, because
// after a panel restart or a host reboot the first deploy would otherwise
// wipe every other tile's jump, and every minute from watchTick, because after
// a host reboot the boot run can precede the containers. Every pause
// container is a routed tile; Route's own reads (pause address, running
// replicas on the env network) run against a collecting VIP, then one Rebuild
// applies the lot. A tile whose row is gone or whose address is not IPv4 is
// logged and skipped. A tile that failed to read for any other reason may
// still have correct kernel rules, so then the others are merged over the
// table and nothing is removed; the next run retries. It runs on its own
// context: a slow Docker never fails boot.
//
// ponytail: re-reads every tile each minute (Docker list plus one inspect per
// replica); fine for tens of tiles, switch to a container-event watch when it
// shows up in the Docker daemon's load. A deploy routing between the reads
// and the Rebuild is overwritten until the next tick.
func (o *Orchestrator) rebuildVIPs(ctx context.Context) error {
	// The minute tick and the async revoke rebuild share this lock, and the
	// grants are read inside it: a revoke can never be overwritten by a
	// rebuild that read the grants before it.
	// ponytail: one lock for every node (deploy.Flow.VIPLock is a plain
	// sync.Locker, no node in it); a slow node holds up the others' rebuild.
	o.vipMu.Lock()
	defer o.vipMu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	return o.eachNode(ctx, o.rebuildNodeVIPs)
}

// nodeVIPs is the VIP table of every node: the tile.VIP the tile leaf holds
// sends each call to the table of the node in its ctx. A table is whatever
// the node's iptables are driven through (the local vip.Table on the
// controller; a worker's, step 3).
type nodeVIPs struct {
	mu     sync.RWMutex
	tables map[string]tile.VIP
}

func newNodeVIPs(tables map[string]tile.VIP) *nodeVIPs {
	return &nodeVIPs{tables: maps.Clone(tables)}
}

// Attach sets or replaces a node's table.
func (n *nodeVIPs) Attach(nodeID string, t tile.VIP) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.tables[nodeID] = t
}

// Detach drops a node's table (the node is offline or gone).
func (n *nodeVIPs) Detach(nodeID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.tables, nodeID)
}

// For is the table of the node in ctx.
func (n *nodeVIPs) For(ctx context.Context) (tile.VIP, error) {
	id, ok := docker.NodeFrom(ctx)
	if !ok {
		return nil, docker.ErrNoNode
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	t, ok := n.tables[id]
	if !ok {
		return nil, fmt.Errorf("%w: no VIP table for node %s", errs.ErrBusy, id)
	}
	return t, nil
}

func (n *nodeVIPs) Set(ctx context.Context, v string, replicas, lan []string) error {
	t, err := n.For(ctx)
	if err != nil {
		return err
	}
	return t.Set(ctx, v, replicas, lan)
}

func (n *nodeVIPs) Remove(ctx context.Context, v string) error {
	t, err := n.For(ctx)
	if err != nil {
		return err
	}
	return t.Remove(ctx, v)
}

// vipFor is the VIP table of the node in ctx. A single table (a test stub,
// or a build without nodeVIPs) serves every node.
func (o *Orchestrator) vipFor(ctx context.Context) (tile.VIP, error) {
	if nv, ok := o.vip.(*nodeVIPs); ok {
		return nv.For(ctx)
	}
	return o.vip, nil
}

// rebuildNodeVIPs is rebuildVIPs for one node (ctx names it), under vipMu.
func (o *Orchestrator) rebuildNodeVIPs(ctx context.Context, n store.Node) error {
	t, err := o.vipFor(ctx)
	if err != nil {
		return err
	}
	r, ok := t.(interface {
		Rebuild(context.Context, map[string]vip.Entry, vip.Base) error
		Merge(context.Context, map[string]vip.Entry, []string, vip.Base) error
	})
	if !ok {
		return nil // a test stub: nothing to declare
	}
	o.sweepEarly(ctx, n.ID)
	pauses, err := o.docker.List(ctx, map[string]string{tile.LabelRole: "pause"})
	if err != nil {
		return err
	}
	base := o.vipBase(ctx, n)
	all := collectVIP{}
	grants := map[string]hostgrant.Set{} // one read per stack
	leaf := tile.New(o.store.Tiles, o.docker, all)
	var failed, drop []string
	skip := func(slug string, err error) {
		slog.Warn("vip: rebuild skips tile", "tile", slug, "err", err)
		if !errors.Is(err, errs.ErrNotFound) && !errors.Is(err, errBadVIPAddr) {
			failed = append(failed, slug)
		}
	}
	for _, p := range pauses {
		id := p.Labels[tile.LabelTile]
		t, err := o.store.Tiles.Get(ctx, id)
		if err != nil {
			skip(id, err)
			if errors.Is(err, errs.ErrNotFound) { // an orphan pause: its VIP is stale
				if d, err := o.docker.Inspect(ctx, p.ID); err == nil {
					drop = append(drop, slices.Collect(maps.Values(d.Networks))...)
				}
			}
			continue
		}
		e, err := o.store.Environments.Get(ctx, t.EnvironmentID)
		if err != nil {
			skip(t.Slug, err)
			continue
		}
		g, ok := grants[t.StackID]
		if !ok {
			if g, err = o.hostgrant.Of(ctx, t.StackID); err != nil {
				skip(t.Slug, err)
				continue
			}
			grants[t.StackID] = g
		}
		if err := leaf.Route(ctx, t, e.Network, deploy.LanLines(g, t)); err != nil {
			skip(t.Slug, err)
		}
	}
	// A tile with no replicas called Remove: partial or not, its VIP goes.
	for v, e := range all {
		if len(e.Replicas) == 0 {
			drop = append(drop, v)
			delete(all, v)
		}
	}
	if len(failed) > 0 {
		if err := r.Merge(ctx, all, drop, base); err != nil {
			return err
		}
		o.dropRouted(n.ID, all)
		return fmt.Errorf("vip: rebuild kept the old rules of %v: could not read them", failed)
	}
	if err := r.Rebuild(ctx, all, base); err != nil {
		return err
	}
	o.dropRouted(n.ID, all)
	return nil
}

// rerouteVIPs is the minute tick's rebuild: a failure is logged, never fatal.
func (o *Orchestrator) rerouteVIPs(ctx context.Context) {
	if err := o.rebuildVIPs(ctx); err != nil {
		slog.Warn("vip: rebuild failed", "err", err)
	}
}

// rerouteVIPsAsync is rerouteVIPs off the request: a slow Docker must not hold
// the admin's approve or revoke. rebuildVIPs logs its own failures.
func (o *Orchestrator) rerouteVIPsAsync(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	o.vipAsync.Add(1)
	go func() {
		defer o.vipAsync.Done()
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		o.rerouteVIPs(ctx)
	}()
}

// vipBase is what the filter table needs beyond the VIPs: the resolvers of
// the host, Caddy's docker0 address, the front proxy and the panel's listen
// address. rebuildVIPs applies it with the VIPs in one restore, so the nat
// rules are never applied from an empty table.
func (o *Orchestrator) vipBase(ctx context.Context, n store.Node) vip.Base {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	rng, err := netip.ParsePrefix(n.RangeCIDR)
	if err != nil {
		slog.Warn("vip: node range does not parse, no filter table", "node", n.Name, "range", n.RangeCIDR)
	}
	b := vip.Base{Range: rng, Cluster: cluster.SupernetPrefix, PanelBind: o.cfg.PanelBind, PanelPort: o.cfg.PanelPort,
		WGIface: mesh.Iface}
	if t, err := mesh.TransitAddr(netip.MustParsePrefix(cluster.ControllerRange)); err == nil {
		b.WGController = t
	}
	id, err := o.controllerID(ctx)
	if err == nil && n.ID != id {
		// A worker: its own resolvers; the controller's Caddy, front proxy
		// and LAN are nothing to it.
		b.Resolvers = worker.Resolvers() // until its facts are in
		if f, ok, _ := o.clusterFacts(ctx, n); ok && len(f.Resolvers) > 0 {
			b.Resolvers = f.Resolvers
		}
		b.HostProxy = slices.Contains(node.Tags(n), node.TagIngress)
		b.Flows = append(o.ingressFlows(ctx, n), o.linkFlows(ctx, n)...)
		return b
	}
	if err == nil {
		b.WGPanelPort = o.cfg.PanelPort
	}
	b.Resolvers = worker.Resolvers()
	if addrs, err := o.domains.ProxyAddrs(ctx); err != nil {
		slog.Warn("vip: proxy address unknown", "err", err)
	} else {
		for _, a := range addrs {
			if ip, err := netip.ParseAddr(a); err == nil && ip.Is4() && !rng.Contains(ip) {
				b.ProxyIP = a
				break
			}
		}
	}
	if raw, err := o.settings.Get(ctx, "trusted_proxies"); err == nil {
		for _, f := range domain.ExpandProxies(splitList(raw), true) {
			// IPv6 ranges are not in this IPv4-only table.
			if p, err := netip.ParsePrefix(f); err == nil && p.Addr().Is4() {
				b.Front = append(b.Front, f)
			} else if a, err := netip.ParseAddr(f); err == nil && a.Is4() {
				b.Front = append(b.Front, f)
			}
		}
	}
	b.Flows = append(o.ingressFlows(ctx, n), o.linkFlows(ctx, n)...)
	return b
}

// ingressFlows are the accepts a node needs for tiles it runs whose ingress
// node is another node: that node's Caddy dials the tile's VIP over the mesh
// (masqueraded to its transit address).
// ponytail: tcp on the domain's port only.
func (o *Orchestrator) ingressFlows(ctx context.Context, n store.Node) []vip.Flow {
	envs, err := o.store.Environments.ListByNode(ctx, n.ID)
	if err != nil {
		return nil
	}
	var out []vip.Flow
	for _, e := range envs {
		ts, err := o.store.Tiles.ListByEnv(ctx, e.ID)
		if err != nil {
			continue
		}
		for _, t := range ts {
			ds, err := o.domains.ListByTile(ctx, t.ID)
			if err != nil || len(ds) == 0 {
				continue
			}
			ing, err := o.IngressFor(ctx, t.ID)
			if err == nil && ing == "" {
				ing, err = o.DefaultIngress(ctx)
			}
			if err != nil || ing == "" || ing == n.ID {
				continue
			}
			in, err := o.store.Nodes.Get(ctx, ing)
			if err != nil {
				continue
			}
			rng, err := netip.ParsePrefix(in.RangeCIDR)
			if err != nil {
				continue
			}
			src, err := mesh.TransitAddr(rng)
			if err != nil {
				continue
			}
			v, ok := o.tileVIP(ctx, n.ID, t.ID)
			if !ok {
				continue
			}
			ports := map[int]bool{}
			for _, d := range ds {
				p := d.ContainerPort
				if p == 0 {
					p = t.ContainerPort
				}
				if p > 0 && !ports[p] {
					ports[p] = true
					out = append(out, vip.Flow{Src: netip.PrefixFrom(src, 32), VIP: v, Port: p})
				}
			}
		}
	}
	return out
}

// earlyLan is tile.Leaf.Early: a starting replica or run container gets its
// tile's lan rules at once, before its health gate (a replica is routed only
// after it), and loses them when it is gone. The address is remembered per
// container: Docker clears it once the container stops, so the way off must
// not inspect. Failures are logged: the tick rebuild still brings the rules a
// minute later and drops the ones whose container is gone (sweepEarly).
//
// Lock order: vipMu -> earlyMu -> Table.mu, never the reverse. Every Allow for
// an early entry runs holding earlyMu and only while the entry is still in
// earlyIPs with that address, so the off path, the on path and sweepEarly
// cannot leave a stale rule behind by interleaving.
func (o *Orchestrator) earlyLan(ctx context.Context, t store.Tile, id string, on bool) {
	if t.HostNetwork {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	e, err := o.store.Environments.Get(ctx, t.EnvironmentID)
	if err != nil {
		return
	}
	// The rules live on the env's node, whatever node the caller's ctx names.
	if ctx, err = o.withEnvNode(ctx, e); err != nil {
		return
	}
	node, _ := docker.NodeFrom(ctx)
	key := earlyKey(node, id)
	tbl, err := o.vipFor(ctx)
	if err != nil {
		return
	}
	a, ok := tbl.(interface {
		Allow(context.Context, string, []string) error
	})
	if !ok {
		return
	}
	if !on {
		o.earlyMu.Lock()
		defer o.earlyMu.Unlock()
		if ent, ok := o.earlyIPs[key]; ok {
			delete(o.earlyIPs, key)
			if err := a.Allow(ctx, ent.ip, nil); err != nil {
				slog.Warn("vip: early lan rules", "tile", t.Slug, "err", err)
			}
		}
		return
	}
	d, err := o.docker.Inspect(ctx, id)
	if err != nil || d.Networks[e.Network] == "" {
		return
	}
	ip := d.Networks[e.Network]
	// The grants are read under the lock: a revoke sweep that runs after this
	// read waits for the lock and then sees the entry.
	o.earlyMu.Lock()
	defer o.earlyMu.Unlock()
	g, err := o.hostgrant.Of(ctx, t.StackID)
	if err != nil {
		slog.Warn("vip: early lan rules", "tile", t.Slug, "err", err)
		return
	}
	lan := deploy.LanLines(g, t)
	if len(lan) == 0 {
		return
	}
	o.earlyIPs[key] = earlyEnt{node: node, id: id, tile: t.ID, ip: ip}
	if err := a.Allow(ctx, ip, lan); err != nil {
		slog.Warn("vip: early lan rules", "tile", t.Slug, "err", err)
	}
}

// earlyEnt is one container holding early lan rules, on one node.
type earlyEnt struct{ node, id, tile, ip string }

// earlyKey keys earlyIPs by node as well as container: a container id is
// only unique on its own daemon.
func earlyKey(node, id string) string { return node + "/" + id }

// sweepEarly runs inside rebuildVIPs (under vipMu), for the node in ctx: an
// early entry whose container is no longer running, or whose tile's grant no
// longer has lan lines, is cleared; the others are re-declared from the
// current grants, so a revoke during a running job cuts its rule at once. The
// reads run outside earlyMu; the Allow runs inside it and only if the entry
// is unchanged.
func (o *Orchestrator) sweepEarly(ctx context.Context, node string) {
	ctx = docker.WithNode(ctx, node)
	tbl, err := o.vipFor(ctx)
	if err != nil {
		return
	}
	a, ok := tbl.(interface {
		Allow(context.Context, string, []string) error
	})
	if !ok {
		return
	}
	o.earlyMu.Lock()
	snap := maps.Clone(o.earlyIPs)
	o.earlyMu.Unlock()
	for key, ent := range snap {
		if ent.node != node {
			continue
		}
		var lan []string
		d, err := o.docker.Inspect(ctx, ent.id)
		gone := errors.Is(err, errs.ErrNotFound) || (err == nil && !d.Running)
		if err != nil && !gone {
			continue // unreadable now: keep, the next tick retries
		}
		if !gone {
			t, err := o.store.Tiles.Get(ctx, ent.tile)
			if err != nil && !errors.Is(err, errs.ErrNotFound) {
				continue
			}
			if err == nil {
				g, gerr := o.hostgrant.Of(ctx, t.StackID)
				if gerr != nil {
					continue
				}
				lan = deploy.LanLines(g, t)
			}
		}
		o.earlyMu.Lock()
		if o.earlyIPs[key] == ent { // the off path or a new owner may have moved on
			if len(lan) == 0 {
				delete(o.earlyIPs, key)
			}
			if err := a.Allow(ctx, ent.ip, lan); err != nil {
				slog.Warn("vip: early lan sweep", "ip", ent.ip, "err", err)
			}
		}
		o.earlyMu.Unlock()
	}
}

// dropRouted forgets the early entries whose address a VIP now lists: the VIP
// rules (and Table.apply, which drops the early rule) take over, so the entry
// is not re-applied each minute and its address is not cleared later under a
// new owner. An address means something only on its own node.
func (o *Orchestrator) dropRouted(node string, all map[string]vip.Entry) {
	routed := map[string]bool{}
	for _, e := range all {
		for _, ip := range e.Replicas {
			routed[ip] = true
		}
	}
	o.earlyMu.Lock()
	defer o.earlyMu.Unlock()
	for key, ent := range o.earlyIPs {
		if ent.node == node && routed[ent.ip] {
			delete(o.earlyIPs, key)
		}
	}
}

// vipLegacy warns at boot, per node, about a legacy iptables backend.
func (o *Orchestrator) vipLegacy(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	_ = o.eachNode(ctx, func(ctx context.Context, n store.Node) error {
		tbl, err := o.vipFor(ctx)
		if err != nil {
			return nil
		}
		t, ok := tbl.(interface{ Legacy(context.Context) bool })
		if !ok {
			return nil // a test stub
		}
		if t.Legacy(ctx) {
			slog.Warn("iptables: DOCKER-USER is missing, the host looks like the legacy backend; "+
				"tiles stay isolated but a tile cannot reach this server's own 80/443", "node", n.Name)
		}
		return nil
	})
}

var errBadVIPAddr = errors.New("not an IPv4 address")

// collectVIP is a tile.VIP that only records what Route would declare.
type collectVIP map[string]vip.Entry

// Set refuses a tile with a non-IPv4 address so Rebuild never sees one: a
// single bad address would fail the whole table.
func (c collectVIP) Set(_ context.Context, v string, replicas, lan []string) error {
	for _, ip := range append([]string{v}, replicas...) {
		if a, err := netip.ParseAddr(ip); err != nil || !a.Is4() {
			return fmt.Errorf("%q: %w", ip, errBadVIPAddr)
		}
	}
	c[v] = vip.Entry{Replicas: replicas, Lan: lan}
	return nil
}

// Remove records the VIP with no replicas; rebuildVIPs drops it from the table.
func (c collectVIP) Remove(_ context.Context, v string) error {
	c[v] = vip.Entry{}
	return nil
}
