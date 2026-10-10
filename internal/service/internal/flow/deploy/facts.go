package deploy

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// snapshot is everything the resolver may see for tile t: its param blocks
// (ParamSnapshot) and every tile of its env, a slice tile as t's binding sees it. A
// managed instance is reached through a slice tile, never by name.
// ponytail: stackr.PROXY_IP and org.backups are not filled; a ref to either
// fails the deploy with the resolver's own message until they are.
func (f *Flow) snapshot(
	ctx context.Context,
	t store.Tile,
	e store.Environment,
	st store.Stack,
) (params.Snapshot, error) {
	s, err := f.ParamSnapshot(ctx, e, st, true)
	if err != nil {
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
	s.Tiles = map[string]params.Source{}
	for _, x := range tiles {
		switch x.Kind {
		case tile.Managed:
			continue
		case tile.Slice:
			src, err := f.sliceSource(ctx, bound, x)
			if err != nil {
				return s, err
			}
			s.Tiles[x.Slug] = src
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

// sliceSource is slice tile x as the consumer's binding sees it: that cred's
// outputs, and the instance's network, which every consumer joins (the
// instance may sit in another env or stack).
func (f *Flow) sliceSource(ctx context.Context, bound map[string]store.Binding, x store.Tile) (params.Source, error) {
	b, ok := bound[x.ID]
	src := params.Source{
		Slice:    true,
		Attached: ok,
	}
	if !ok {
		return src, nil
	}
	out, err := managed.Outputs(b)
	if err != nil {
		return src, err
	}
	src.Outputs = out
	p, err := f.Managed.GetProvision(ctx, b.ProvisionID)
	if err != nil {
		return src, err
	}
	src.Network = managed.Network(p.InstanceID)
	return src, nil
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

// ownParams fills what env e of stack st reads implicitly: Env, Tier, EnvParams
// and OrgParams.
//   - a PR env reads its own env scope (seeded from the stack's pr block) and
//     the org's pr block;
//   - an org with no tiers reads its org scope;
//   - an env named like a tier reads that tier's block;
//   - an off-tier env of a tiered org reads no org block (OrgParams stays nil).
//
// tiers is the org's ladder, bottom first.
func (f *Flow) ownParams(ctx context.Context, e store.Environment, st store.Stack, tiers []store.Tier, secrets bool) (params.Snapshot, error) {
	envScope, orgScope, tier := ownScopes(e, st, tiers)
	s := params.Snapshot{Env: e.Slug, Tier: tier}
	var err error
	if s.EnvParams, err = f.Params.Values(ctx, envScope, secrets); err != nil {
		return s, err
	}
	if orgScope != nil {
		s.OrgParams, err = f.Params.Values(ctx, *orgScope, secrets)
	}
	return s, err
}

// ownScopes are the scopes env e reads implicitly (see ownParams) and its
// tier slug; the org scope is nil when it reads none.
func ownScopes(e store.Environment, st store.Stack, tiers []store.Tier) (env params.Scope, org *params.Scope, tier string) {
	switch {
	case e.Type == environment.Ephemeral:
		return params.Scope{Kind: "env", ID: e.ID}, &params.Scope{Kind: "org_pr", ID: st.OrgID}, ""
	case len(tiers) == 0:
		return params.Scope{Kind: "env", ID: e.ID}, &params.Scope{Kind: "org", ID: st.OrgID}, ""
	}
	for _, t := range tiers {
		if t.Slug == e.Slug {
			return params.Scope{Kind: "env", ID: e.ID}, &params.Scope{Kind: "tier", ID: t.ID}, t.Slug
		}
	}
	return params.Scope{Kind: "env", ID: e.ID}, nil, ""
}

// ParamSnapshot is the one place a consumer env's param view is built: its
// own blocks (ownParams) and Other, every block a [x] ref may name, with the
// lock of each. A tiered env takes its tier's lock, an off-tier one its own;
// PR blocks are never locked. A locked block carries no values.
func (f *Flow) ParamSnapshot(ctx context.Context, e store.Environment, st store.Stack, secrets bool) (params.Snapshot, error) {
	tiers, err := f.Tiers.List(ctx, st.OrgID)
	if err != nil {
		return params.Snapshot{}, err
	}
	s, err := f.ownParams(ctx, e, st, tiers, secrets)
	if err != nil {
		return s, err
	}
	s.Other = map[string]params.Block{}
	block := func(key string, locked bool, sc params.Scope) error {
		b := params.Block{Locked: locked}
		if !locked {
			var err error
			if b.Values, err = f.Params.Values(ctx, sc, secrets); err != nil {
				return err
			}
		}
		s.Other[key] = b
		return nil
	}
	if err := block("stack:pr", false, params.Scope{Kind: "stack_pr", ID: st.ID}); err != nil {
		return s, err
	}
	if err := block("org:pr", false, params.Scope{Kind: "org_pr", ID: st.OrgID}); err != nil {
		return s, err
	}
	lock := map[string]bool{}
	for _, t := range tiers {
		lock[t.Slug] = t.Locked
		if err := block("org:"+t.Slug, t.Locked, params.Scope{Kind: "tier", ID: t.ID}); err != nil {
			return s, err
		}
	}
	es, err := f.Envs.List(ctx, st.ID)
	if err != nil {
		return s, err
	}
	for _, x := range es {
		if x.Type == environment.Ephemeral {
			continue
		}
		locked, tiered := lock[x.Slug]
		if !tiered {
			locked = x.Locked
		}
		if err := block("stack:"+x.Slug, locked, params.Scope{Kind: "env", ID: x.ID}); err != nil {
			return s, err
		}
	}
	return s, nil
}

func envMap(blob string) (map[string]string, error) {
	m := map[string]string{}
	if strings.TrimSpace(blob) == "" {
		return m, nil
	}
	return m, json.Unmarshal([]byte(blob), &m)
}
