package managedtiles

// Resolving an infra path to what it names. Two things layer on top of the
// plain org:stack:env:slug walk in infrapath.go:
//
//   - a path may name a *slice* (a logical db, a bucket) as well as the
//     instance it was cut from, so one address grammar covers both;
//   - a path may be *relative* to where the caller is standing, resolved
//     narrowest scope first, so a linked directory can say "app-db".
//
// Resolution is also bounded by what the caller may see. That is not
// decoration: this turns a name into an id, so an unbounded resolver lets any
// key confirm that another org's stack, env and database exist by guessing at
// them. A path outside the caller's orgs fails exactly like one that does not
// exist.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// notFoundError marks "nothing here", so ResolveTarget can tell a miss, try
// the other reading of the path, from a problem worth reporting, such as a
// name that is both a slice and an instance. Without the distinction the
// actionable error is swallowed and reported as a flat "not found".
type notFoundError struct{ msg string }

func (e notFoundError) Error() string { return e.msg }

func notFound(format string, a ...any) error {
	return notFoundError{fmt.Sprintf(format, a...)}
}

func isNotFound(err error) bool {
	var n notFoundError
	return errors.As(err, &n)
}

// Target kinds.
const (
	TargetInstance = "instance"
	TargetSlice    = "slice"
)

// InfraTarget is what a path resolved to. Callers switch on Kind rather than
// on which pointer happens to be nil.
type InfraTarget struct {
	Kind      string
	Instance  *repo.Tile      // always set, a slice's instance is its provider
	Provision *repo.Provision // set only for TargetSlice
	Path      string          // the fully-qualified form, for prompts and output
}

// ResolveScope bounds a resolution to what its caller may see and where it is
// standing.
type ResolveScope struct {
	// AllowedOrgs limits the search to these org ids. A nil map means no
	// limit, which is only correct for server-side callers acting on their own
	// behalf, never for anything driven by a request. Use SystemScope to say
	// so deliberately.
	AllowedOrgs map[string]bool
	// EnvID is the caller's linked environment. Set, it enables relative
	// paths; empty, only absolute paths resolve.
	EnvID string
}

// SystemScope resolves without an access check, for internal callers such as
// the config applier. Named so that granting unrestricted access is greppable
// rather than an omitted argument.
func SystemScope() ResolveScope { return ResolveScope{} }

func (s ResolveScope) permits(orgID string) bool {
	return s.AllowedOrgs == nil || s.AllowedOrgs[orgID]
}

// ResolveTarget turns an infra path into the instance or slice it names.
//
// A relative reading is tried first (narrowest scope first) and then an
// absolute one. If both resolve to different things the path is ambiguous and
// this errors rather than picking, the same address would otherwise mean
// different things depending on which directory you ran it from.
func ResolveTarget(ctx context.Context, store repo.Store, path string, scope ResolveScope) (*InfraTarget, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("empty infra path")
	}
	var rel *InfraTarget
	var relErr error
	if scope.EnvID != "" {
		rel, relErr = resolveRelative(ctx, store, path, scope)
	}
	abs, absErr := resolveAbsolute(ctx, store, path, scope)
	switch {
	case rel != nil && abs != nil && rel.Path != abs.Path:
		return nil, fmt.Errorf("%q is ambiguous: %s from here, or %s read as a full path; say which",
			path, rel.Path, abs.Path)
	case rel != nil:
		return rel, nil
	case abs != nil:
		return abs, nil
	}
	// Neither reading found anything. If one failed for a reason other than
	// absence (a name that is both a slice and an instance, say) report that
	// instead of a flat "not found": it is the actionable one.
	for _, err := range []error{absErr, relErr} {
		if err != nil && !isNotFound(err) {
			return nil, err
		}
	}
	return nil, notFound("nothing named %q", path)
}

// resolveRelative reads the path as relative to the caller's environment:
// "name", "env:name" for a sibling env, or "stack:env:name" within the org.
func resolveRelative(ctx context.Context, store repo.Store, path string, scope ResolveScope) (*InfraTarget, error) {
	env, err := store.GetEnvironment(ctx, scope.EnvID)
	if err != nil || env == nil {
		return nil, notFound("no environment %q", scope.EnvID)
	}
	stack, err := store.GetStack(ctx, env.StackID)
	if err != nil || stack == nil {
		return nil, fmt.Errorf("no stack for environment %q", scope.EnvID)
	}
	if !scope.permits(stack.OrgID) {
		return nil, notFound("nothing named %q", path)
	}
	parts := strings.Split(path, ":")
	switch len(parts) {
	case 1:
		// Narrowest first: the env, then instances scoped to the stack, then to
		// the org. Same order stackconf.resolveFrom already uses.
		return lookupOutward(ctx, store, stack, env, parts[0])
	case 2: // env:name, a sibling environment of the same stack
		sib, err := store.GetEnvironmentBySlug(ctx, stack.ID, parts[0])
		if err != nil || sib == nil {
			return nil, notFound("no environment %q", parts[0])
		}
		return lookupInEnv(ctx, store, stack, sib, parts[1])
	case 3: // stack:env:name, another stack in the same org
		st, err := store.GetStackBySlug(ctx, stack.OrgID, parts[0])
		if err != nil || st == nil {
			return nil, notFound("no stack %q", parts[0])
		}
		e, err := store.GetEnvironmentBySlug(ctx, st.ID, parts[1])
		if err != nil || e == nil {
			return nil, notFound("no environment %q", parts[1])
		}
		return lookupInEnv(ctx, store, st, e, parts[2])
	}
	return nil, notFound("nothing named %q", path)
}

// resolveAbsolute reads the path as org:[stack:[env:]]name, the form InfraPath
// prints.
func resolveAbsolute(ctx context.Context, store repo.Store, path string, scope ResolveScope) (*InfraTarget, error) {
	parts := strings.Split(path, ":")
	if len(parts) < 2 || len(parts) > 4 {
		return nil, notFound("invalid infra path %q", path)
	}
	org, err := store.GetOrgBySlug(ctx, parts[0])
	if err != nil || org == nil || !scope.permits(org.ID) {
		// Deliberately the same error whether the org is missing or merely not
		// the caller's: the difference is exactly what must not leak.
		return nil, notFound("nothing named %q", path)
	}
	switch len(parts) {
	case 4: // org:stack:env:name
		stack, err := store.GetStackBySlug(ctx, org.ID, parts[1])
		if err != nil || stack == nil {
			return nil, notFound("no stack %q", parts[1])
		}
		env, err := store.GetEnvironmentBySlug(ctx, stack.ID, parts[2])
		if err != nil || env == nil {
			return nil, notFound("no environment %q", parts[2])
		}
		return lookupInEnv(ctx, store, stack, env, parts[3])
	case 3: // org:stack:name, an instance scoped to the stack
		stack, err := store.GetStackBySlug(ctx, org.ID, parts[1])
		if err != nil || stack == nil {
			return nil, notFound("no stack %q", parts[1])
		}
		return instanceAtScope(ctx, store, org, stack, parts[2], "stack")
	default: // org:name, an instance scoped to the org
		stacks, err := store.ListStacksByOrg(ctx, org.ID)
		if err != nil {
			return nil, err
		}
		for i := range stacks {
			if t, _ := instanceAtScope(ctx, store, org, &stacks[i], parts[1], "org"); t != nil {
				return t, nil
			}
		}
		return nil, notFound("nothing named %q", path)
	}
}

// lookupOutward searches the env, then the stack, then the org, the narrowest
// thing wearing the name wins, which is what "app-db" means from inside a
// linked directory.
func lookupOutward(ctx context.Context, store repo.Store, stack *repo.Stack, env *repo.Environment, name string) (*InfraTarget, error) {
	if t, err := lookupInEnv(ctx, store, stack, env, name); err == nil && t != nil {
		return t, nil
	}
	org, err := store.GetOrg(ctx, stack.OrgID)
	if err != nil || org == nil {
		return nil, fmt.Errorf("no org for stack %q", stack.Slug)
	}
	if t, _ := instanceAtScope(ctx, store, org, stack, name, "stack"); t != nil {
		return t, nil
	}
	stacks, err := store.ListStacksByOrg(ctx, org.ID)
	if err != nil {
		return nil, err
	}
	for i := range stacks {
		if t, _ := instanceAtScope(ctx, store, org, &stacks[i], name, "org"); t != nil {
			return t, nil
		}
	}
	return nil, notFound("nothing named %q", name)
}

// lookupInEnv finds a slice or an env-scoped instance by name in one
// environment. A name that is both is an error rather than a guess: the two
// are different things to drop.
func lookupInEnv(ctx context.Context, store repo.Store, stack *repo.Stack, env *repo.Environment, name string) (*InfraTarget, error) {
	org, err := store.GetOrg(ctx, stack.OrgID)
	if err != nil || org == nil {
		return nil, fmt.Errorf("no org for stack %q", stack.Slug)
	}
	slice, err := sliceBySlug(ctx, store, env.ID, name)
	if err != nil {
		return nil, err
	}
	tile, err := store.GetTileBySlug(ctx, env.ID, name)
	if err != nil {
		tile = nil
	}
	if tile != nil && !tile.IsManaged() {
		tile = nil // only shared instances are addressable as infra
	}
	switch {
	case slice != nil && tile != nil:
		return nil, fmt.Errorf("%q is both a slice and an instance in %s; rename one", name, env.Slug)
	case slice != nil:
		inst, err := store.GetTile(ctx, slice.InstanceTileID)
		if err != nil || inst == nil {
			return nil, fmt.Errorf("slice %q has no instance", name)
		}
		return &InfraTarget{
			Kind: TargetSlice, Instance: inst, Provision: slice,
			Path: strings.Join([]string{org.Slug, stack.Slug, env.Slug, ResourceSlug(inst, slice)}, ":"),
		}, nil
	case tile != nil:
		return &InfraTarget{
			Kind: TargetInstance, Instance: tile,
			Path: InfraPath(tile.ScopeKind, org.Slug, stack.Slug, env.Slug, tile.Slug),
		}, nil
	}
	return nil, notFound("nothing named %q in %s", name, env.Slug)
}

// instanceAtScope finds an instance carrying the given slug at one scope kind
// within a stack.
func instanceAtScope(ctx context.Context, store repo.Store, org *repo.Org, stack *repo.Stack, slug, scopeKind string) (*InfraTarget, error) {
	tiles, err := store.ListTilesByStack(ctx, stack.ID)
	if err != nil {
		return nil, err
	}
	t, err := findScoped(tiles, slug, scopeKind, slug)
	if err != nil || t == nil {
		return nil, notFound("nothing named %q", slug)
	}
	return &InfraTarget{
		Kind: TargetInstance, Instance: t,
		Path: InfraPath(t.ScopeKind, org.Slug, stack.Slug, "", t.Slug),
	}, nil
}

// sliceBySlug finds the provision in an env whose resource slug is name.
func sliceBySlug(ctx context.Context, store repo.Store, envID, name string) (*repo.Provision, error) {
	ps, err := store.ListProvisionsByEnv(ctx, envID)
	if err != nil {
		return nil, err
	}
	insts := map[string]*repo.Tile{}
	for i := range ps {
		if ps[i].Status == "orphaned" {
			continue
		}
		id := ps[i].InstanceTileID
		if _, seen := insts[id]; !seen {
			insts[id], _ = store.GetTile(ctx, id)
		}
		if insts[id] == nil {
			continue
		}
		if ResourceSlug(insts[id], &ps[i]) == name {
			return &ps[i], nil
		}
	}
	return nil, nil
}
