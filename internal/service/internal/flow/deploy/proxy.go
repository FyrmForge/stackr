package deploy

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/s3"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// ProxyConfig is the Syncer's Build: every domain row, grouped by tile, with
// the facts the rows lack (running replica names, the protect cascade, the
// tile's params for basic-auth refs), plus the admin-made external routes ext,
// turned into one whole Caddy config.
func (f *Flow) ProxyConfig(ctx context.Context, in domain.Install, ext []domain.Route) (json.RawMessage, error) {
	routes, err := f.tileRoutes(ctx)
	if err != nil {
		return nil, err
	}
	return domain.Build(in, routes, ext, nil)
}

// Placement answers where a tile is served and runs: its resolved ingress
// node id, the node its containers run on, and its VIP on the env network
// (used as the upstream when the two differ).
type Placement func(tileID string) (ingressNode, node, vip string)

// ProxyConfigs is ProxyConfig per ingress node: one config for each node in
// the result, keyed by node id. Configs that built are returned with the
// error naming the ones that did not.
func (f *Flow) ProxyConfigs(ctx context.Context, in domain.Install, ext []domain.NodeRoute, place Placement) (map[string]json.RawMessage, error) {
	routes, err := f.tileRoutes(ctx)
	if err != nil {
		return nil, err
	}
	if place != nil {
		for i := range routes {
			routes[i].IngressNode, routes[i].Node, routes[i].VIP = place(routes[i].TileID)
		}
	}
	return domain.BuildAll(in, routes, ext, nil)
}

func (f *Flow) tileRoutes(ctx context.Context) ([]domain.TileRoute, error) {
	ds, err := f.Domains.List(ctx)
	if err != nil {
		return nil, err
	}
	byTile := map[string][]store.Domain{}
	var order []string
	for _, d := range ds {
		if _, ok := byTile[d.TileID]; !ok {
			order = append(order, d.TileID)
		}
		byTile[d.TileID] = append(byTile[d.TileID], d)
	}
	server, err := f.Settings.Defaults(ctx)
	if err != nil {
		return nil, err
	}
	var routes []domain.TileRoute
	for _, id := range order {
		r, err := f.tileRoute(ctx, id, server)
		if err != nil {
			return nil, err
		}
		r.Domains = byTile[id]
		for _, d := range r.Domains {
			if x, err := domain.ExtrasOf(d); err == nil && x.ForwardAuth != nil {
				// A public host needs no names; a private one that is not an org
				// tile then fails closed in the build.
				if _, pub := s3.PublicAddr(x.ForwardAuth.URL); !pub {
					r.OrgNames = f.OrgReplicaNames(ctx, id)
					break
				}
			}
		}
		routes = append(routes, r)
	}
	return routes, nil
}

func (f *Flow) tileRoute(ctx context.Context, id string, server settings.Settings) (domain.TileRoute, error) {
	r := domain.TileRoute{TileID: id}
	t, err := f.Tiles.Get(ctx, id)
	if err != nil {
		return r, err
	}
	e, err := f.Envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return r, err
	}
	st, err := f.Stacks.Get(ctx, t.StackID)
	if err != nil {
		return r, err
	}
	o, err := f.Orgs.Get(ctx, st.OrgID)
	if err != nil {
		return r, err
	}
	cs, err := f.Tiles.Replicas(ctx, t)
	if err != nil {
		return r, err
	}
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		up := c.Name
		if c.HostNetwork { // decided by the container, not the row: the old bridged one keeps its name until the swap
			up = domain.HostUpstream
		}
		if !slices.Contains(r.Upstreams, up) {
			r.Upstreams = append(r.Upstreams, up)
		}
	}
	r.HealthPath = t.HealthPath
	levels := []settings.Settings{server}
	for _, blob := range []string{o.Settings, st.Settings, e.Settings} {
		s, err := settings.Parse(blob)
		if err != nil {
			return r, err
		}
		levels = append(levels, s)
	}
	if p := settings.Resolve(levels...); p.Protect {
		r.Protect = &domain.BasicAuth{User: p.ProtectUser, Password: p.ProtectPassword}
	}
	// Domain refs see params only (never a secret, never a tile output).
	snap, err := f.ParamSnapshot(ctx, e, st, false)
	if err != nil {
		return r, err
	}
	rr := params.NewResolver(snap)
	r.Expand = func(s string) (string, error) { return rr.Expand(params.InDomain, s) }
	return r, nil
}

// OrgReplicaNames are the container names (lower case, any state) of every
// tile in the org of tileID: the private hosts a forward-auth URL may name.
// Best effort: a tile whose replicas cannot be listed is skipped, which only
// makes a forward-auth URL naming it refuse (fail closed), never open.
func (f *Flow) OrgReplicaNames(ctx context.Context, tileID string) []string {
	t, err := f.Tiles.Get(ctx, tileID)
	if err != nil {
		return nil
	}
	st, err := f.Stacks.Get(ctx, t.StackID)
	if err != nil {
		return nil
	}
	stacks, err := f.Stacks.List(ctx, st.OrgID)
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range stacks {
		tiles, err := f.Tiles.ListByStack(ctx, s.ID)
		if err != nil {
			continue
		}
		for _, x := range tiles {
			cs, err := f.Tiles.Replicas(ctx, x)
			if err != nil {
				slog.Warn("forward auth: list replicas", "tile", x.ID, "err", err)
				continue
			}
			for _, c := range cs {
				out = append(out, strings.ToLower(c.Name))
			}
		}
	}
	return out
}
