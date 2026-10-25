package service

import (
	"context"
	"slices"
	"sync"
	"time"
)

// dnsBannerTTL is how long a banner answer is reused; page loads and the
// banner's own refresh inside it cost no lookups. Tests set it to 0.
var dnsBannerTTL = 2 * time.Minute

type bannerKey struct {
	o   *Orchestrator
	env string
}

type bannerHit struct {
	at time.Time
	m  DNSMove
}

var bannerCache sync.Map // bannerKey -> bannerHit

// EnvDNSBanner is the env's hosts that still resolve away from the ingress
// node they enter by: Hand the hand-managed ones (Records says what to set),
// Managed the held ones a user unticked in a move. A held host that now
// resolves to its node is released, so stackr manages it again.
func (o *Orchestrator) EnvDNSBanner(ctx context.Context, envID string) (DNSMove, error) {
	k := bannerKey{o, envID}
	if v, ok := bannerCache.Load(k); ok && time.Since(v.(bannerHit).at) < dnsBannerTTL {
		return v.(bannerHit).m, nil
	}
	m, err := o.envDNSBanner(ctx, envID)
	if err == nil {
		bannerCache.Store(k, bannerHit{time.Now(), m})
	}
	return m, err
}

func (o *Orchestrator) envDNSBanner(ctx context.Context, envID string) (DNSMove, error) {
	out := DNSMove{Hand: []DNSHost{}, Managed: []DNSHost{}}
	tiles, err := o.store.Tiles.ListByEnv(ctx, envID)
	if err != nil {
		return out, err
	}
	def, err := o.DefaultIngress(ctx)
	if err != nil {
		return out, err
	}
	held := o.DNSHeld(ctx)
	var all DNSMove
	for _, t := range tiles {
		node, err := o.IngressFor(ctx, t.ID)
		if err != nil {
			return out, err
		}
		if node == "" {
			node = def
		}
		ds, err := o.store.Domains.ListByTile(ctx, t.ID)
		if err != nil {
			return out, err
		}
		for _, d := range ds {
			h, managed, err := o.dnsHost(ctx, t.ID, d.Host, node, node)
			if err != nil {
				return out, err
			}
			switch {
			case !managed:
				all.Hand = append(all.Hand, h)
			case slices.Contains(held, d.Host):
				all.Managed = append(all.Managed, h)
			}
		}
	}
	all = o.CheckDNS(ctx, all)
	var matched []string
	for _, h := range all.Hand {
		if !h.OK {
			out.Hand = append(out.Hand, h)
		}
	}
	for _, h := range all.Managed {
		if h.OK {
			matched = append(matched, h.Host)
		} else {
			out.Managed = append(out.Managed, h)
		}
	}
	if len(matched) > 0 {
		if err := o.ReleaseDNS(ctx, matched); err != nil {
			return out, err
		}
	}
	return out, nil
}

// releaseMatchedHolds lets stackr manage a held host again once it resolves
// to the node it enters by, with no page open: the records sync and the
// daily tick run it.
func (o *Orchestrator) releaseMatchedHolds(ctx context.Context) error {
	held := o.DNSHeld(ctx)
	if len(held) == 0 {
		return nil
	}
	ds, err := o.domains.List(ctx)
	if err != nil {
		return err
	}
	def, err := o.DefaultIngress(ctx)
	if err != nil {
		return err
	}
	var m DNSMove
	for _, d := range ds {
		if !slices.Contains(held, d.Host) {
			continue
		}
		node, err := o.IngressFor(ctx, d.TileID)
		if err != nil {
			return err
		}
		if node == "" {
			node = def
		}
		h, managed, err := o.dnsHost(ctx, d.TileID, d.Host, node, node)
		if err != nil {
			return err
		}
		if managed {
			m.Managed = append(m.Managed, h)
		}
	}
	var matched []string
	for _, h := range o.CheckDNS(ctx, m).Managed {
		if h.OK {
			matched = append(matched, h.Host)
		}
	}
	if len(matched) == 0 {
		return nil
	}
	return o.ReleaseDNS(ctx, matched)
}
