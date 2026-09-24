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
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	ltraffic "github.com/FyrmForge/stackr/internal/service/internal/leaf/traffic"
)

// DefaultPath is the host conntrack table as a host-network process sees it.
const DefaultPath = "/proc/net/nf_conntrack"

type Flow struct {
	Tiles   *tile.Leaf
	Envs    *environment.Leaf
	Domains *domain.Leaf
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
