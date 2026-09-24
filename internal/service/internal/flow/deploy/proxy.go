package deploy

import (
	"context"
	"encoding/json"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// ProxyConfig is the Syncer's Build: every domain row, grouped by tile, with
// the facts the rows lack (running replica names, the protect cascade, the
// tile's params for basic-auth refs), turned into one whole Caddy config.
func (f *Flow) ProxyConfig(ctx context.Context, in domain.Install) (json.RawMessage, error) {
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
		routes = append(routes, r)
	}
	return domain.Build(in, routes, nil)
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
		if c.State == "running" {
			r.Upstreams = append(r.Upstreams, c.Name)
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
	var snap params.Snapshot
	for _, sc := range []struct {
		dst  *map[string]params.Value
		kind string
		id   string
	}{
		{&snap.EnvParams, "env", e.ID},
		{&snap.StackParams, "stack", st.ID},
		{&snap.OrgParams, "org", o.ID},
	} {
		if *sc.dst, err = f.Params.Values(ctx, params.Scope{Kind: sc.kind, ID: sc.id}, false); err != nil {
			return r, err
		}
	}
	rr := params.NewResolver(snap)
	r.Expand = func(s string) (string, error) { return rr.Expand(params.InDomain, s) }
	return r, nil
}
