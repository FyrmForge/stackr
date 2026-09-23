package stackconf

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// resolveFrom turns a slice's dotted from: address into its instance tile.
// Partial forms fill in from the current context:
//
//	instance                : visible from this env (own env, stack- or org-scoped)
//	stack.instance          : stack in the current org (falls back to org.instance)
//	org.stack.instance      : fully qualified stack scope
//	org.stack.env.instance  : fully qualified env scope
func (a Applier) resolveFrom(ctx context.Context, stack *repo.Stack, env *repo.Environment, from string) (*repo.Tile, error) {
	store := a.Planner.Store
	org, err := store.GetOrg(ctx, stack.OrgID)
	if err != nil || org == nil {
		return nil, fmt.Errorf("org of stack %s not found", stack.Slug)
	}
	segs := strings.Split(from, ".")
	try := func(colonPath string) *repo.Tile {
		t, _ := managedtiles.ResolveInfraPath(ctx, store, colonPath)
		return t
	}
	switch len(segs) {
	case 1:
		// Own env first, then stack scope, then org scope, narrowest wins.
		for _, p := range []string{
			org.Slug + ":" + stack.Slug + ":" + env.Slug + ":" + segs[0],
			org.Slug + ":" + stack.Slug + ":" + segs[0],
			org.Slug + ":" + segs[0],
		} {
			if t := try(p); t != nil {
				return t, nil
			}
		}
	case 2:
		// stack.instance in the current org, else org.instance.
		for _, p := range []string{
			org.Slug + ":" + segs[0] + ":" + segs[1],
			segs[0] + ":" + segs[1],
		} {
			if t := try(p); t != nil {
				return t, nil
			}
		}
	case 3:
		if t := try(segs[0] + ":" + segs[1] + ":" + segs[2]); t != nil {
			return t, nil
		}
	case 4:
		if t := try(strings.Join(segs, ":")); t != nil {
			return t, nil
		}
	}
	return nil, fmt.Errorf("from %q: no such instance visible from %s/%s", from, stack.Slug, env.Slug)
}

// createSlice provisions (or adopts) a config-declared slice. The config key
// is the resource slug; consumers are whichever tiles reference it.
func (a Applier) createSlice(ctx context.Context, stack *repo.Stack, env *repo.Environment, slug string, tc TileConf) error {
	if a.Slices == nil {
		return nil // no database service wired (tests, spec-only construction)
	}
	inst, err := a.resolveFrom(ctx, stack, env, tc.From)
	if err != nil {
		return fmt.Errorf("slice %s: %w", slug, err)
	}
	// An ephemeral (PR) env never adopts: it gets a fresh uniquified copy under
	// the same reference slug, so previews can't touch another env's data.
	p, err := a.Slices.Cut(ctx, inst, env, slug, tc.SliceName(slug), tc.Public, env.Type == "ephemeral")
	if err != nil {
		return fmt.Errorf("slice %s: %w", slug, err)
	}
	return a.stampSlicePolicy(ctx, p, tc)
}

// updateSlice applies in-place slice changes: on_remove policy and (s3) the
// public flag. From/name changes never reach here, diffSlice replaces.
func (a Applier) updateSlice(ctx context.Context, stack *repo.Stack, env *repo.Environment, slug string, tc TileConf, fields map[string]bool) error {
	if a.Slices == nil {
		return nil
	}
	inst, p, err := a.findSlice(ctx, env, slug)
	if err != nil || p == nil {
		return fmt.Errorf("slice %s: not found in %s", slug, env.Slug)
	}
	if fields["public"] {
		// Every consumer's row moves, not just the representative one this
		// found: the policy belongs to the bucket, and flipping one row left
		// the others claiming the old visibility for ever.
		if err := a.Slices.SetPublic(ctx, inst, p, tc.Public); err != nil {
			return fmt.Errorf("slice %s: %w", slug, err)
		}
	}
	return a.stampSlicePolicy(ctx, p, tc)
}

// stampSlicePolicy persists on_remove onto the row: by the time the entry
// leaves the file, the declaration that carried the policy is gone.
func (a Applier) stampSlicePolicy(ctx context.Context, p *repo.Provision, tc TileConf) error {
	want := ""
	if tc.OnRemove == "drop" {
		want = "drop"
	}
	if p.OnRemove == want {
		return nil
	}
	p.OnRemove = want
	return a.Planner.Store.UpdateProvision(ctx, p)
}

// removeSlice is the strict-mode removal of a slice entry: the held on_remove
// policy decides between orphaning (keep, default) and destroying the data.
func (a Applier) removeSlice(ctx context.Context, env *repo.Environment, slug string) (bool, error) {
	if a.Slices == nil {
		return false, nil
	}
	inst, p, err := a.findSlice(ctx, env, slug)
	if err != nil {
		return false, err
	}
	if p == nil {
		return false, nil // not a slice (or already gone)
	}
	if p.OnRemove == "drop" {
		return true, a.Slices.Drop(ctx, inst, p.DBName)
	}
	// Detach by consumer id, which a config-declared slice does not have; the
	// row is orphaned directly.
	p.ConsumerTileID, p.Status = "", "orphaned"
	return true, a.Planner.Store.UpdateProvision(ctx, p)
}

// findSlice locates the live slice a slug names in an env: its instance and a
// representative (non-orphaned) provision row.
func (a Applier) findSlice(ctx context.Context, env *repo.Environment, slug string) (*repo.Tile, *repo.Provision, error) {
	store := a.Planner.Store
	res, err := store.ListResourcesByEnv(ctx, env.ID)
	if err != nil {
		return nil, nil, err
	}
	for i := range res {
		if res[i].Slug != slug {
			continue
		}
		inst, err := store.GetTile(ctx, res[i].ProviderTileID)
		if err != nil || inst == nil {
			return nil, nil, fmt.Errorf("slice %s: providing instance not found", slug)
		}
		ps, err := store.ListProvisionsByInstance(ctx, inst.ID)
		if err != nil {
			return nil, nil, err
		}
		for j := range ps {
			if ps[j].DBName == res[i].Name && ps[j].Status != "orphaned" {
				return inst, &ps[j], nil
			}
		}
		return inst, nil, nil
	}
	return nil, nil, nil
}

// syncBindings grants the tile access to every env resource its variables
// reference, so `${{ tile.<slice>.<OUTPUT> }}` resolves at deploy, and
// revokes what it no longer references. Runs after ReplaceTileVars.
//
// panel/API-provisioned slices manage bindings through their own
// attach/detach paths; this only reconciles reference-derived ones, and a
// binding both paths agree on is idempotent either way.
func (a Applier) syncBindings(ctx context.Context, t *repo.Tile, tc TileConf) error {
	store := a.Planner.Store
	res, err := store.ListResourcesByEnv(ctx, t.EnvironmentID)
	if err != nil || len(res) == 0 {
		return err
	}
	referenced := map[string]bool{}
	for _, val := range tc.Env {
		for _, body := range varref.Refs(val) {
			if ref, err := varref.Parse(body); err == nil && ref.Scope == "tile" && ref.Slug != "" {
				referenced[ref.Slug] = true
			}
		}
	}
	now := time.Now().UTC()
	for i := range res {
		if !referenced[res[i].Slug] {
			continue
		}
		if err := store.CreateBinding(ctx, &repo.ResourceBinding{ResourceID: res[i].ID,
			ConsumerTileID: t.ID, CreatedAt: now}); err != nil {
			return err
		}
	}
	return nil
}
