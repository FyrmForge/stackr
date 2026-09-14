package managedtiles

import (
	"context"
	"fmt"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// InfraPath is the human-facing address of a shared instance. The segment count
// encodes the scope: org-scoped instances are addressed org:slug, stack-scoped
// org:stack:slug, env-scoped org:stack:env:slug.
func InfraPath(scopeKind, orgSlug, stackSlug, envSlug, tileSlug string) string {
	switch scopeKind {
	case "org":
		return orgSlug + ":" + tileSlug
	case "stack":
		return orgSlug + ":" + stackSlug + ":" + tileSlug
	default: // env
		return orgSlug + ":" + stackSlug + ":" + envSlug + ":" + tileSlug
	}
}

// ResolveInfraPath turns an InfraPath back into its instance tile. The walk
// depth follows the segment count; org/stack-scoped instances live in some env
// but are addressed at the higher scope, so those two cases scan for the tile.
func ResolveInfraPath(ctx context.Context, store repo.Store, path string) (*repo.Tile, error) {
	parts := strings.Split(path, ":")
	org, err := store.GetOrgBySlug(ctx, parts[0])
	if err != nil || org == nil {
		return nil, fmt.Errorf("no org %q", parts[0])
	}
	switch len(parts) {
	case 4: // org:stack:env:slug, env-scoped, direct
		stack, err := store.GetStackBySlug(ctx, org.ID, parts[1])
		if err != nil || stack == nil {
			return nil, fmt.Errorf("no stack %q", parts[1])
		}
		env, err := store.GetEnvironmentBySlug(ctx, stack.ID, parts[2])
		if err != nil || env == nil {
			return nil, fmt.Errorf("no env %q", parts[2])
		}
		t, err := store.GetTileBySlug(ctx, env.ID, parts[3])
		if err != nil || t == nil {
			return nil, fmt.Errorf("no instance %q", parts[3])
		}
		return t, nil
	case 3: // org:stack:slug, stack-scoped, scan the stack
		stack, err := store.GetStackBySlug(ctx, org.ID, parts[1])
		if err != nil || stack == nil {
			return nil, fmt.Errorf("no stack %q", parts[1])
		}
		tiles, err := store.ListTilesByStack(ctx, stack.ID)
		if err != nil {
			return nil, err
		}
		return findScoped(tiles, parts[2], "stack", path)
	case 2: // org:slug, org-scoped, scan every stack in the org
		stacks, err := store.ListStacksByOrg(ctx, org.ID)
		if err != nil {
			return nil, err
		}
		for i := range stacks {
			tiles, err := store.ListTilesByStack(ctx, stacks[i].ID)
			if err != nil {
				return nil, err
			}
			if t, _ := findScoped(tiles, parts[1], "org", path); t != nil {
				return t, nil
			}
		}
		return nil, fmt.Errorf("no instance %q", path)
	}
	return nil, fmt.Errorf("invalid infra path %q", path)
}

// findScoped returns the first tile matching slug at the given scope. // first match wins on a slug collision within the scope, vanishingly rare and
// an error there would be more confusing than picking one.
func findScoped(tiles []repo.Tile, slug, scope, path string) (*repo.Tile, error) {
	for i := range tiles {
		if tiles[i].Slug == slug && tiles[i].ScopeKind == scope {
			return &tiles[i], nil
		}
	}
	return nil, fmt.Errorf("no instance %q", path)
}
