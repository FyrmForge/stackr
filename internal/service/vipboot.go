package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
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
		Rebuild(context.Context, map[string][]string) error
		Merge(context.Context, map[string][]string) error
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
	all := collectVIP{}
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
		if err := leaf.Route(ctx, t, e.Network); err != nil {
			skip(t.Slug, err)
		}
	}
	if len(failed) > 0 {
		if err := r.Merge(ctx, all); err != nil {
			return err
		}
		return fmt.Errorf("vip: rebuild kept the old rules of %v: could not read them", failed)
	}
	return r.Rebuild(ctx, all)
}

// rerouteVIPs is the minute tick's rebuild: a failure is logged, never fatal.
func (o *Orchestrator) rerouteVIPs(ctx context.Context) {
	if err := o.rebuildVIPs(ctx); err != nil {
		slog.Warn("vip: rebuild failed", "err", err)
	}
}

var errBadVIPAddr = errors.New("not an IPv4 address")

// collectVIP is a tile.VIP that only records what Route would declare.
type collectVIP map[string][]string

// Set refuses a tile with a non-IPv4 address so Rebuild never sees one: a
// single bad address would fail the whole table.
func (c collectVIP) Set(_ context.Context, vip string, replicas []string) error {
	for _, ip := range append([]string{vip}, replicas...) {
		if a, err := netip.ParseAddr(ip); err != nil || !a.Is4() {
			return fmt.Errorf("%q: %w", ip, errBadVIPAddr)
		}
	}
	c[vip] = replicas
	return nil
}

func (collectVIP) Remove(context.Context, string) error { return nil }
