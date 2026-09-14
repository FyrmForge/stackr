package varref

import (
	"context"
	"sort"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Output is one name a source publishes.
type Output struct {
	Name   string
	Kind   string // endpoint | variable | resource
	Secret bool
}

// Source is one thing a tile may reference, with the names it publishes. The
// scope-wide values come as two sources, Slug "vars" and Slug "secrets", so the
// expression a suggestion inserts is right without threading the secret flag
// through Expr.
// Values are deliberately absent: the catalogue drives autocomplete, which must
// be safe to serve to a caller that may not read secrets.
type Source struct {
	Scope   string // tile | stack | org | stackr
	Slug    string // source slug, or the vars/secrets bucket
	Kind    string
	Outputs []Output
}

// Expr renders the reference expression for one of a source's outputs.
func (s Source) Expr(name string) string {
	if s.Slug == "" {
		return "${{ " + s.Scope + "." + name + " }}"
	}
	return "${{ " + s.Scope + "." + s.Slug + "." + name + " }}"
}

// Catalogue lists what a consumer tile may reference: sibling tiles in its
// environment, the managed resources actually bound to it, and its stack's and
// org's variables. Unbound resources are left out, advertising them would
// suggest references that are guaranteed to fail.
func Catalogue(ctx context.Context, store repo.Store, consumerTileID string) ([]Source, error) {
	t, err := store.GetTile(ctx, consumerTileID)
	if err != nil || t == nil {
		return nil, err
	}
	stack, err := store.GetStack(ctx, t.StackID)
	if err != nil || stack == nil {
		return nil, err
	}
	var out []Source
	add := func(s Source) {
		sort.Slice(s.Outputs, func(i, j int) bool { return s.Outputs[i].Name < s.Outputs[j].Name })
		out = append(out, s)
	}

	tiles, err := store.ListTilesByEnv(ctx, t.EnvironmentID)
	if err != nil {
		return nil, err
	}
	for i := range tiles {
		if tiles[i].ID == t.ID || tiles[i].IsVolume() {
			continue
		}
		var outs []Output
		if tiles[i].Kind == "service" && tiles[i].ContainerPort != 0 {
			for _, n := range []string{"STACKR_PRIVATE_DOMAIN", "STACKR_INTERNAL_PORT", "STACKR_INTERNAL_URL", "STACKR_PUBLIC_URL", "STACKR_PUBLIC_DOMAIN"} {
				outs = append(outs, Output{Name: n, Kind: "endpoint"})
			}
		}
		vars, err := store.ListVariables(ctx, repo.OwnerTile, tiles[i].ID)
		if err != nil {
			return nil, err
		}
		for _, v := range vars {
			outs = append(outs, Output{Name: v.Name, Kind: "variable", Secret: v.Secret})
		}
		kind := tiles[i].Kind
		if tiles[i].IsManaged() {
			kind = tiles[i].Engine
		}
		add(Source{Scope: "tile", Slug: tiles[i].Slug, Kind: kind, Outputs: outs})
	}

	binds, err := store.BindingsForConsumer(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	for _, b := range binds {
		res, err := store.GetResource(ctx, b.ResourceID)
		if err != nil || res == nil || res.EnvironmentID != t.EnvironmentID {
			continue
		}
		outs, err := store.ListOutputs(ctx, res.ID)
		if err != nil {
			return nil, err
		}
		var meta []Output
		for _, o := range outs {
			meta = append(meta, Output{Name: o.Name, Kind: "resource", Secret: o.Secret})
		}
		add(Source{Scope: "tile", Slug: res.Slug, Kind: res.Kind, Outputs: meta})
	}

	for _, sc := range []struct{ scope, ownerKind, ownerID string }{
		{"stack", repo.OwnerStack, stack.ID},
		{"org", repo.OwnerOrg, stack.OrgID},
	} {
		vars, err := store.ListVariables(ctx, sc.ownerKind, sc.ownerID)
		if err != nil {
			return nil, err
		}
		buckets := map[string][]Output{}
		for _, v := range vars {
			b := BucketVars
			if v.Secret {
				b = BucketSecrets
			}
			buckets[b] = append(buckets[b], Output{Name: v.Name, Kind: "variable", Secret: v.Secret})
		}
		for _, b := range []string{BucketVars, BucketSecrets} {
			if len(buckets[b]) > 0 {
				add(Source{Scope: sc.scope, Slug: b, Kind: "variables", Outputs: buckets[b]})
			}
		}
	}
	// The platform scope is a fixed set, so it is always offered, including
	// before the proxy has recorded anything. Autocomplete is where someone
	// discovers these exist, and hiding them until an env happens to be attached
	// would make them look conditional when the names never change.
	add(Source{Scope: "stackr", Kind: "server", Outputs: []Output{
		{Name: PlatformProxyIP, Kind: "variable"},
		{Name: PlatformProxyCIDR, Kind: "variable"},
	}})

	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Slug < out[j].Slug
	})
	return out, nil
}
