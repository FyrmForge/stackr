// Package traffic is the 5 s sampling tick: which IP is which tile (tile
// leaf) and which is the proxy (its container, domain leaf; every stackr
// network's gateway, environment leaf), the host conntrack table (the panel is host-network, so
// /proc/net/nf_conntrack is the host's), one leaf/traffic Sample. A read,
// not a container op, so it runs inline on the scheduler, never as a job.
package traffic

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"sort"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	ltraffic "github.com/FyrmForge/stackr/internal/service/internal/leaf/traffic"
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
}

// Tick samples once. An unreadable table is skipped quietly (Check said so
// at boot) before any Docker call.
func (f *Flow) Tick(ctx context.Context) error {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return nil
	}
	ips, err := f.Tiles.Addresses(ctx)
	if err != nil {
		return err
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
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	f.Traffic.Sample(ips, data, now())
	return nil
}

// Check is the boot warning, "" when the table looks usable: the file must
// exist (some kernels lack procfs conntrack) and carry byte counters
// (net.netfilter.nf_conntrack_acct=1). An empty table says nothing either way.
func Check(path string) string {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "traffic: no conntrack table; tile traffic stays empty (kernel without CONFIG_NF_CONNTRACK_PROCFS?)"
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
