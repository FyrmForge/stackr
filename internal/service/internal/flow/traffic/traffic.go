// Package traffic is the 5 s sampling tick: which IP is which tile (tile
// leaf) and which is the proxy (its container, domain leaf; every stackr
// network's gateway, environment leaf), the host conntrack table (the panel is host-network, so
// /proc/net/nf_conntrack is the host's; a kernel without that file is
// asked over netlink by conntrack -L), one leaf/traffic Sample. A read,
// not a container op, so it runs inline on the scheduler, never as a job.
package traffic

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"sort"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	ltraffic "github.com/FyrmForge/stackr/internal/service/internal/leaf/traffic"
	"github.com/FyrmForge/stackr/internal/worker"
)

// DefaultPath is the host conntrack table as a host-network process sees it.
const DefaultPath = "/proc/net/nf_conntrack"

type Flow struct {
	Tiles   *tile.Leaf
	Envs    *environment.Leaf
	Stacks  *stack.Leaf
	Domains *domain.Leaf
	Managed *managed.Leaf
	Traffic *ltraffic.Leaf
	Path    string           // the conntrack file
	Now     func() time.Time // nil = time.Now
	// Remote is every worker's GET /v1/traffic by node id; a node that did
	// not answer is absent. nil = the controller alone.
	Remote func(ctx context.Context) map[string]worker.Traffic

	// last is each worker's latest answer. A missed read reuses it: same
	// cumulative counters, so that tick shows a gap instead of the node's
	// connections counting in full again on the next answer. Tick runs on
	// the scheduler only, one at a time.
	last map[string]seenDump
}

type seenDump struct {
	t  worker.Traffic
	at time.Time
}

// staleAfter drops a worker's last answer (a node gone for good).
// ponytail: a node back after this counts its open connections in full once.
const staleAfter = 5 * time.Minute

// Tick samples once. An unreadable table is skipped quietly (Check said so
// at boot) before any Docker call.
func (f *Flow) Tick(ctx context.Context) error {
	data, err := read(ctx, f.Path)
	if err != nil && f.Remote == nil {
		return nil
	}
	ips, err := f.Tiles.Addresses(ctx)
	if err != nil {
		return err
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	// Workers: their tiles join the map, and every table keeps only what its
	// node counts, so a cross-node connection is not counted twice.
	remote := f.remote(ctx, now())
	all := map[string]bool{}
	for ip := range ips {
		all[ip] = true
	}
	for _, t := range remote {
		for ip := range t.Tiles {
			all[ip] = true
		}
	}
	isTile := func(ip string) bool { return all[ip] }
	data = ltraffic.Own(data, ips, isTile)
	for _, t := range remote {
		data = append(data, ltraffic.Own([]byte(t.Lines), t.Tiles, isTile)...)
		for ip, id := range t.Tiles {
			ips[ip] = id
		}
	}
	// Ingress: Caddy reaches a tile from its own container on the ingress
	// network; anything host-side (the host-network panel, docker's userland
	// port proxy) arrives from the network's gateway.
	proxy, err := f.Domains.ProxyAddrs(ctx)
	if err != nil {
		return err
	}
	gws, err := f.Envs.Gateways(ctx)
	if err != nil {
		return err
	}
	for _, ip := range append(proxy, gws...) {
		ips[ip] = ltraffic.Proxy
	}
	f.Traffic.Sample(ips, data, now())
	return nil
}

// remote is this tick's worker tables, a missed one standing in with its
// last answer until staleAfter.
func (f *Flow) remote(ctx context.Context, now time.Time) map[string]worker.Traffic {
	if f.Remote == nil {
		return nil
	}
	if f.last == nil {
		f.last = map[string]seenDump{}
	}
	for id, t := range f.Remote(ctx) {
		f.last[id] = seenDump{t, now}
	}
	out := map[string]worker.Traffic{}
	for id, s := range f.last {
		if now.Sub(s.at) > staleAfter {
			delete(f.last, id)
			continue
		}
		out[id] = s.t
	}
	return out
}

// read is the conntrack table: the procfs file, or, on a kernel built
// without it (newer ones), the same lines from conntrack -L over netlink.
// Both are key=value lines the leaf parses alike.
func read(ctx context.Context, path string) ([]byte, error) { return worker.ReadConntrack(ctx, path) }

// Check is the boot warning, "" when the table looks usable: it must be
// readable (the file, or conntrack -L) and carry byte counters
// (net.netfilter.nf_conntrack_acct=1). An empty table says nothing either way.
func Check(path string) string {
	data, err := read(context.Background(), path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "traffic: no conntrack table, and conntrack -L failed; tile traffic stays empty"
	case err != nil:
		return "traffic: cannot read the conntrack table: " + err.Error()
	case len(bytes.TrimSpace(data)) > 0 && !bytes.Contains(data, []byte("bytes=")):
		return "traffic: conntrack has no byte counters; set net.netfilter.nf_conntrack_acct=1 (stackr-install does)"
	}
	return ""
}

// Edges is the latest snapshot as one env's canvas draws it: consumer <->
// instance lanes renamed to the consumer's slice tile (its card), then
// only the lanes with a tile of the env at one end.
func (f *Flow) Edges(ctx context.Context, envID string) ([]ltraffic.Edge, error) {
	ts, err := f.Tiles.List(ctx, envID)
	if err != nil {
		return nil, err
	}
	in := map[string]bool{}
	bind := map[ltraffic.Pair]string{}
	name := map[string]string{
		ltraffic.Proxy:    ltraffic.Proxy,
		ltraffic.Internet: ltraffic.Internet,
	}
	for _, t := range ts {
		in[t.ID] = true
		name[t.ID] = t.Slug
		ps, err := f.Managed.ForConsumer(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			m, err := f.Managed.Get(ctx, p.InstanceID)
			if err != nil {
				return nil, err
			}
			// ponytail: conntrack sees consumer <-> instance, not which
			// database; a consumer on two slices of one instance draws its
			// lane on the last. Per-slice needs the engine's own stats.
			bind[ltraffic.Pair{From: t.ID, To: m.TileID}] = p.TileID
		}
	}
	var out []ltraffic.Edge
	for p, v := range ltraffic.Slices(f.Traffic.Snapshot(), bind) {
		if in[p.From] || in[p.To] {
			out = append(out, ltraffic.Edge{From: p.From, To: p.To, BPS: v})
		}
	}
	for i := range out {
		out[i].FromName = f.name(ctx, name, out[i].From)
		out[i].ToName = f.name(ctx, name, out[i].To)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out, nil
}

// name is the id's slug in this env, stack/env/slug for a tile of another
// env (a slice's instance), the id itself when nothing is known.
func (f *Flow) name(ctx context.Context, known map[string]string, id string) string {
	if n, ok := known[id]; ok {
		return n
	}
	n := id
	if t, err := f.Tiles.Get(ctx, id); err == nil {
		n = t.Slug
		if e, err := f.Envs.Get(ctx, t.EnvironmentID); err == nil {
			if s, err := f.Stacks.Get(ctx, e.StackID); err == nil {
				n = s.Slug + "/" + e.Slug + "/" + t.Slug
			}
		}
	}
	known[id] = n
	return n
}
