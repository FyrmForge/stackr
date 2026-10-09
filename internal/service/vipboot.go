package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/vip"
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
	r, ok := o.vip.(interface {
		Rebuild(context.Context, map[string]vip.Entry, vip.Base) error
		Merge(context.Context, map[string]vip.Entry, vip.Base) error
	})
	if !ok {
		return nil // a test stub: nothing to declare
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	pauses, err := o.docker.List(ctx, map[string]string{tile.LabelRole: "pause"})
	if err != nil {
		return err
	}
	base := o.vipBase(ctx)
	all := collectVIP{}
	grants := map[string]hostgrant.Set{} // one read per stack
	leaf := tile.New(o.store.Tiles, o.docker, all)
	var failed []string
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
	if len(failed) > 0 {
		if err := r.Merge(ctx, all, base); err != nil {
			return err
		}
		return fmt.Errorf("vip: rebuild kept the old rules of %v: could not read them", failed)
	}
	return r.Rebuild(ctx, all, base)
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
func (o *Orchestrator) vipBase(ctx context.Context) vip.Base {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	b := vip.Base{Range: docker.EnvRange, PanelBind: o.cfg.PanelBind}
	b.PanelPort, _ = strconv.Atoi(installspec.PanelPort)
	for _, p := range resolvConfs {
		if raw, err := os.ReadFile(p); err == nil {
			for _, r := range resolvers(string(raw)) {
				if !slices.Contains(b.Resolvers, r) {
					b.Resolvers = append(b.Resolvers, r)
				}
			}
		}
	}
	if addrs, err := o.domains.ProxyAddrs(ctx); err != nil {
		slog.Warn("vip: proxy address unknown", "err", err)
	} else {
		for _, a := range addrs {
			if ip, err := netip.ParseAddr(a); err == nil && ip.Is4() && !docker.EnvRange.Contains(ip) {
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
	return b
}

// vipLegacy warns once at boot about a legacy iptables backend.
func (o *Orchestrator) vipLegacy(ctx context.Context) {
	t, ok := o.vip.(interface{ Legacy(context.Context) bool })
	if !ok {
		return // a test stub
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if t.Legacy(ctx) {
		slog.Warn("iptables: DOCKER-USER is missing, the host looks like the legacy backend; " +
			"tiles stay isolated but a tile cannot reach this server's own 80/443")
	}
}

// resolvConfs are read and joined: on a systemd-resolved host the first is the
// loopback stub and Docker hands containers the second.
var resolvConfs = []string{"/etc/resolv.conf", "/run/systemd/resolve/resolv.conf"}

// resolvers are the IPv4 nameservers of a resolv.conf, loopback ones left
// out: Docker dials those from the host, not from the container.
func resolvers(conf string) []string {
	var out []string
	for _, line := range strings.Split(conf, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		if a, err := netip.ParseAddr(f[1]); err == nil && a.Is4() && !a.IsLoopback() {
			out = append(out, f[1])
		}
	}
	return out
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

func (collectVIP) Remove(context.Context, string) error { return nil }
