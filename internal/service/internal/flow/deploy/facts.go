package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// snapshot is everything the resolver may see for tile t: the three param
// scopes and every tile of its env, managed instances included.
// ponytail: stackr.PROXY_IP and org.backups are not filled; a ref to either
// fails the deploy with the resolver's own message until they are.
func (f *Flow) snapshot(
	ctx context.Context,
	t store.Tile,
	e store.Environment,
	st store.Stack,
) (params.Snapshot, error) {
	var s params.Snapshot
	var err error
	if s.EnvParams, err = f.Params.Values(ctx, params.Scope{Kind: "env", ID: e.ID}, true); err != nil {
		return s, err
	}
	if s.StackParams, err = f.Params.Values(ctx, params.Scope{Kind: "stack", ID: st.ID}, true); err != nil {
		return s, err
	}
	if s.OrgParams, err = f.Params.Values(ctx, params.Scope{Kind: "org", ID: st.OrgID}, true); err != nil {
		return s, err
	}

	mine, err := f.Managed.ForConsumer(ctx, t.ID)
	if err != nil {
		return s, err
	}
	slices := map[string]store.Provision{} // by instance id
	for _, p := range mine {
		slices[p.InstanceID] = p
	}

	tiles, err := f.Tiles.List(ctx, e.ID)
	if err != nil {
		return s, err
	}
	s.Tiles = map[string]params.Source{}
	for _, x := range tiles {
		if x.Kind == tile.Managed {
			// step 7b task 4 replaces this: the slice tile is the source, an
			// instance is reached from its own env only until then.
			m, err := f.Managed.GetByTile(ctx, x.ID)
			if errors.Is(err, errs.ErrNotFound) {
				continue
			}
			if err != nil {
				return s, err
			}
			p, attached := slices[m.ID]
			s.Tiles[x.Slug] = params.Source{
				Managed:  true,
				Attached: attached,
				Outputs:  managed.Outputs(p),
			}
			continue
		}
		src, err := f.endpoint(ctx, x)
		if err != nil {
			return s, err
		}
		s.Tiles[x.Slug] = src
		if x.ID == t.ID {
			s.Self = src
		}
	}
	return s, nil
}

// endpoint is a service or image tile's built-in outputs.
func (f *Flow) endpoint(ctx context.Context, x store.Tile) (params.Source, error) {
	ds, err := f.Domains.ListByTile(ctx, x.ID)
	if err != nil {
		return params.Source{}, err
	}
	ep := params.Endpoint{Alias: x.Slug, Port: x.ContainerPort, Protocol: x.EndpointProtocol}
	for _, d := range ds {
		ep.Domains = append(ep.Domains, params.Domain{Host: d.Host, HTTPS: d.HTTPS, Redirect: d.RedirectTo != ""})
	}
	return params.Source{Outputs: ep.Outputs()}, nil
}

func envMap(blob string) (map[string]string, error) {
	m := map[string]string{}
	if strings.TrimSpace(blob) == "" {
		return m, nil
	}
	return m, json.Unmarshal([]byte(blob), &m)
}
