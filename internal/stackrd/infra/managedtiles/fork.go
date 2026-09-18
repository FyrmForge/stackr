package managedtiles

// Forking a slice: copy a live logical database or bucket into a fresh slice
// on the same instance, so a migration can be rehearsed against real data.
//
// Fork is provision + copy. The provisioning half is CloneSlice's, unchanged,
// everything new here is the copy half and the naming around it. The copy
// itself never leaves the instance: postgres pipes dump into restore inside
// the container, s3 copies object-side on the server.

import (
	"context"
	"fmt"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// CanFork reports whether the engine can copy a slice's data, the way
// CanProvision reports whether it can cut one.
func CanFork(engine string) bool { return Engines[engine].Fork != nil }

// ForkSlice provisions a fresh slice alongside src on the same instance and
// copies src's data into it.
//
// name is the new database/bucket name ("" derives it from src, and Provision
// uniquifies it against the instance); slug is the fork's reference name ("" derives
// it from src's, uniquified against the env).
//
// The fork lands in the source's environment with no consumer, structurally
// the same row a config-declared slice has, so it appears in the instance
// drawer and can be attached to a consumer later through the existing flow.
// It is never public: a fork exists to be experimented on, and inheriting a
// source bucket's world-readability would publish that experiment.
func (s *Service) ForkSlice(ctx context.Context, instance *repo.Tile, src *repo.Provision, slug, name string) (*repo.Provision, error) {
	eng := Engines[instance.Engine]
	if eng.Fork == nil {
		return nil, fmt.Errorf("engine %q cannot fork %ss", instance.Engine, sliceNoun(instance.Engine))
	}
	if name == "" {
		name = src.DBName + "-fork"
	}
	if slug == "" {
		// ResourceSlug, not src.ResourceSlug: the field is empty on slices cut
		// from the instance drawer, and only the helper knows the fallback.
		slug = ResourceSlug(instance, src) + "-fork"
	}
	slug, err := s.uniqueResourceSlug(ctx, src.EnvID, slug)
	if err != nil {
		return nil, err
	}
	// no free-space pre-flight. A fork doubles the slice's data on
	// the instance volume; the failure mode is a mid-copy error on a fresh,
	// droppable database, not damage to the source. Add a size + statfs check
	// if that stops being cheap to recover from.
	dst, err := s.CloneSlice(ctx, instance, src.EnvID, slug, name, false)
	if err != nil {
		return nil, err
	}
	if err := eng.Fork(s, ctx, instance, src, dst); err != nil {
		// An empty fork is worse than no fork, it looks like a copy that
		// worked. Drop it so a retry starts clean. Safe because the name was
		// uniquified instance-wide, so it cannot match a sibling's slice.
		_ = s.DropDB(ctx, instance, dst.DBName)
		return nil, fmt.Errorf("copying %s %q: %w", sliceNoun(instance.Engine), src.DBName, err)
	}
	return dst, nil
}

// uniqueResourceSlug suffixes -2, -3… until the slug is free in the
// environment, which managed_resources requires (UNIQUE(env, slug)).
//
// one GetTile per distinct instance in the env, cached. Fine at the
// row counts a single env holds; a slug column on the provision would avoid it
// if that ever stops being true.
func (s *Service) uniqueResourceSlug(ctx context.Context, envID, base string) (string, error) {
	ps, err := s.store.ListProvisionsByEnv(ctx, envID)
	if err != nil {
		return "", err
	}
	insts := map[string]*repo.Tile{}
	taken := map[string]bool{}
	for i := range ps {
		id := ps[i].InstanceTileID
		if _, seen := insts[id]; !seen {
			insts[id], _ = s.store.GetTile(ctx, id)
		}
		if insts[id] == nil {
			continue
		}
		taken[ResourceSlug(insts[id], &ps[i])] = true
	}
	slug := base
	for n := 2; taken[slug]; n++ {
		slug = fmt.Sprintf("%s-%d", base, n)
	}
	return slug, nil
}
