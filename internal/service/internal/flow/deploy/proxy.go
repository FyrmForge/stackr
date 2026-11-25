package deploy

import (
	"context"
	"encoding/json"
	"fmt"
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
	r.HealthPath = t.HealthHTTPPath
	r.HealthStatus, r.HealthIntervalS = t.HealthHTTPStatus, t.HealthHTTPIntervalS
	r.HealthTimeoutS, r.HealthRetries = t.HealthHTTPTimeoutS, t.HealthHTTPRetries
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
	// r.Expand reads a gate login only, so it sees secrets: the password is
	// hashed before it reaches the proxy config. Host names are checked
	// elsewhere, under the strict InDomain rule.
	snap, err := f.ParamSnapshot(ctx, e, st, true)
	if err != nil {
		return r, err
	}
	rr := params.NewResolver(snap)
	r.Expand = func(s string) (string, error) { return rr.Expand(params.InGate, s) }
	return r, nil
}

// GateInfo is a domain's gate as the panel shows it: User as written (a ref
// stays a ref, a resolved secret never leaves), and why the address is
// locked ("" when it is not). Never the password.
type GateInfo struct{ User, Locked string }

// Gates reports every domain's gate with one fingerprint and at most one
// secret snapshot per env. A domain that cannot be read is locked with the
// error as its reason; it never fails the others. Domains with no gate are
// absent. tiles is the loaded tiles the domains sit on. The panel polls this
// every few seconds, so a verdict is kept until the env's params, tiers, envs
// or the domain's own gate change (gateFingerprint): no secret is decrypted
// while nothing moved.
func (f *Flow) Gates(ctx context.Context, ds []store.Domain, tiles map[string]store.Tile) map[string]GateInfo {
	type envGate struct {
		fp  string
		ex  domain.Expand
		err error
		ok  bool // ex was built
	}
	out := map[string]GateInfo{}
	envs := map[string]*envGate{} // env id -> its fingerprint and expander, built when first needed
	for _, d := range ds {
		x, err := domain.ExtrasOf(d)
		if err == nil && x.BasicAuth == nil {
			continue
		}
		if err != nil {
			out[d.ID] = GateInfo{Locked: err.Error()}
			continue
		}
		t, found := tiles[d.TileID]
		if !found {
			out[d.ID] = GateInfo{User: x.BasicAuth.User, Locked: "its tile is gone"}
			continue
		}
		g := envs[t.EnvironmentID]
		if g == nil {
			g = &envGate{}
			g.fp, g.err = f.gateFingerprint(ctx, t)
			envs[t.EnvironmentID] = g
		}
		key := g.fp + "\x00" + d.ProxyJSON
		if g.err == nil {
			if v, hit := f.gateCache.Load(d.ID); hit && v.(gateVerdict).key == key {
				out[d.ID] = v.(gateVerdict).info
				continue
			}
		}
		if g.err == nil && !g.ok {
			g.ex, g.err = f.gateExpand(ctx, t)
			g.ok = g.err == nil
		}
		if g.err != nil {
			out[d.ID] = GateInfo{User: x.BasicAuth.User, Locked: g.err.Error()}
			continue
		}
		_, _, locked := domain.GateLogin(*x.BasicAuth, g.ex)
		out[d.ID] = GateInfo{User: x.BasicAuth.User, Locked: locked}
		f.gateCache.Store(d.ID, gateVerdict{key, out[d.ID]})
	}
	return out
}

// gateVerdict is a kept Gates answer and what it was computed from.
type gateVerdict struct {
	key  string
	info GateInfo
}

// gateFingerprint is what a gate's resolution reads besides the domain: the
// params stamp (any param write), the env and its stack, and the lock of every
// tier and env (a locked block resolves to nothing).
func (f *Flow) gateFingerprint(ctx context.Context, t store.Tile) (string, error) {
	e, err := f.Envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return "", err
	}
	st, err := f.Stacks.Get(ctx, t.StackID)
	if err != nil {
		return "", err
	}
	stamp, err := f.Params.Stamp(ctx)
	if err != nil {
		return "", err
	}
	tiers, err := f.Tiers.List(ctx, st.OrgID)
	if err != nil {
		return "", err
	}
	es, err := f.Envs.List(ctx, st.ID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|%s|%s", stamp, e.ID, e.Slug, e.Type, st.ID)
	for _, x := range tiers {
		fmt.Fprintf(&b, "|t:%s:%s:%t", x.ID, x.Slug, x.Locked)
	}
	for _, x := range es {
		fmt.Fprintf(&b, "|e:%s:%s:%t", x.ID, x.Slug, x.Locked)
	}
	return b.String(), nil
}

func (f *Flow) gateExpand(ctx context.Context, t store.Tile) (domain.Expand, error) {
	e, err := f.Envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return nil, err
	}
	st, err := f.Stacks.Get(ctx, t.StackID)
	if err != nil {
		return nil, err
	}
	snap, err := f.ParamSnapshot(ctx, e, st, true)
	if err != nil {
		return nil, err
	}
	rr := params.NewResolver(snap)
	return func(s string) (string, error) { return rr.Expand(params.InGate, s) }, nil
}

// GateRefs are the references a gate in env envID may name as its password:
// every variable and secret the env resolves, its own and the tier or org block
// it inherits (ownScopes), as "${{ params.<collection>.<name> }}". Names only;
// a secret is never read.
func (f *Flow) GateRefs(ctx context.Context, envID string) ([]string, error) {
	e, err := f.Envs.Get(ctx, envID)
	if err != nil {
		return nil, err
	}
	st, err := f.Stacks.Get(ctx, e.StackID)
	if err != nil {
		return nil, err
	}
	tiers, err := f.Tiers.List(ctx, st.OrgID)
	if err != nil {
		return nil, err
	}
	envScope, orgScope, _ := ownScopes(e, st, tiers)
	scopes := []params.Scope{envScope}
	if orgScope != nil {
		scopes = append(scopes, *orgScope)
	}
	var refs []string
	for _, sc := range scopes {
		ps, err := f.Params.Masked(ctx, sc)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			refs = append(refs, "${{ params."+p.Collection+"."+p.Name+" }}")
		}
	}
	slices.Sort(refs)
	return slices.Compact(refs), nil
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
