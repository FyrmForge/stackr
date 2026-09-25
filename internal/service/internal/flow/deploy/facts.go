package deploy

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// snapshot is everything the resolver may see for tile t: the three param
// scopes and every tile of its env, a slice tile as t's binding sees it. A
// managed instance is reached through a slice tile, never by name.
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

	bound, err := f.Managed.Bound(ctx, t.ID) // by slice tile id
	if err != nil {
		return s, err
	}

	tiles, err := f.Tiles.List(ctx, e.ID)
	if err != nil {
		return s, err
	}
	s.Env = e.Slug
	s.Tiles = map[string]params.Source{}
	for _, x := range tiles {
		switch x.Kind {
		case tile.Managed:
			continue
		case tile.Slice:
			b, ok := bound[x.ID]
			out, err := managed.Outputs(b)
			if err != nil {
				return s, err
			}
			// step 7b task 5 fills Network: the instance's network, which
			// the consumer joins when the instance sits in another stack.
			s.Tiles[x.Slug] = params.Source{
				Slice:    true,
				Attached: ok,
				Outputs:  out,
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
