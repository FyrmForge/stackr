package managedtiles

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envutil"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// This file mirrors provision rows into the managed-resource model the
// reference resolver reads: one resource per logical slice (database/bucket),
// its published outputs, and one binding per consumer. The provision row stays
// the source of truth for credentials, mirroring is cheaper and less risky
// than rewriting provisioning itself, and it keeps cloning working off the
// provision.
//
// fold the provision row into the resource once nothing else reads
// `provisions` (the legacy-drop migration at the end of the rewrite).

// ResourceSlug is the name a provisioned slice answers to in a reference:
// ${{ tile.<slug>.DATABASE_URL }}. A config-declared slice carries its config
// key on the row; otherwise the slug is derived, qualified by the instance
// because a bucket or db name is only unique per instance, while the slug must
// be unique per environment.
func ResourceSlug(instance *repo.Tile, p *repo.Provision) string {
	if p.ResourceSlug != "" {
		return p.ResourceSlug
	}
	return instance.Slug + "-" + p.DBName
}

// Ref is the reference expression a consumer puts in a variable to read one of
// a slice's outputs.
func Ref(instance *repo.Tile, p *repo.Provision, output string) string {
	return "${{ tile." + ResourceSlug(instance, p) + "." + output + " }}"
}

// DefaultOutput is the output a consumer wants when it names no specific one.
func DefaultOutput(engine string) string { return Engines[engine].PrimaryOutput }

// sliceNoun names what this engine's provisioned units are called, for copy
// that has to read right for both a logical db and a bucket.
func sliceNoun(engine string) string {
	if n := Engines[engine].SliceNoun; n != "" {
		return n
	}
	return "slice"
}

// SyncResource creates or refreshes the resource behind a provision and binds
// its consumer. Idempotent per (instance, slice): AttachExisting adds a second
// provision row for the same slice, which must contribute a binding only,
// a second resource row would collide on UNIQUE(environment_id, slug).
func (s *Service) SyncResource(ctx context.Context, instance *repo.Tile, p *repo.Provision) error {
	slug := ResourceSlug(instance, p)
	now := time.Now().UTC()
	existing, err := s.store.ListResourcesByEnv(ctx, p.EnvID)
	if err != nil {
		return err
	}
	// Identity is (provider, slice name), not the slug: a config-declared
	// slice's slug is its config key, and a key rename must move the existing
	// resource, not mint a second one beside it.
	var res *repo.ManagedResource
	for i := range existing {
		if existing[i].ProviderTileID == instance.ID && existing[i].Name == p.DBName {
			res = &existing[i]
			break
		}
	}
	// A slice renamed under the same key (name: cart -> cart2 on cart-db)
	// leaves the old, detached resource holding the slug, and (env, slug) is
	// unique. Take that row over when this slice has none, and drop it when
	// it does: its bindings went at detach, consumers rebind on apply.
	for i := range existing {
		if existing[i].Slug != slug || (res != nil && existing[i].ID == res.ID) {
			continue
		}
		if res == nil {
			res = &existing[i]
		} else if err := s.store.DeleteResource(ctx, existing[i].ID); err != nil {
			return err
		}
	}
	if res == nil {
		res = &repo.ManagedResource{ID: uuid.NewString(), EnvironmentID: p.EnvID,
			ProviderTileID: instance.ID, Name: p.DBName, Slug: slug, Kind: instance.Engine,
			Status: p.Status, Public: p.Public, CreatedAt: now, UpdatedAt: now}
		if err := s.store.CreateResource(ctx, res); err != nil {
			return err
		}
	} else {
		res.Slug, res.Name, res.ProviderTileID, res.Kind, res.Status, res.Public, res.UpdatedAt = slug, p.DBName, instance.ID, instance.Engine, p.Status, p.Public, now
		if err := s.store.UpdateResource(ctx, res); err != nil {
			return err
		}
	}
	for _, o := range s.outputsFor(ctx, instance, p) {
		o.ResourceID = res.ID
		if err := s.store.UpsertOutput(ctx, &o); err != nil {
			return err
		}
	}
	if p.ConsumerTileID == "" {
		return nil
	}
	return s.store.CreateBinding(ctx, &repo.ResourceBinding{ResourceID: res.ID,
		ConsumerTileID: p.ConsumerTileID, CreatedAt: now})
}

// outputsFor is the published surface of a slice. Admin credentials are never
// among them: a consumer gets its own role/bucket keys, nothing that would let
// it reach another consumer's data.
func (s *Service) outputsFor(ctx context.Context, instance *repo.Tile, p *repo.Provision) []repo.ResourceOutput {
	if outputs := Engines[instance.Engine].Outputs; outputs != nil {
		return outputs(s, ctx, instance, p)
	}
	return nil
}

// AutoInjectVars maps every output a slice publishes to a reference at it,
// what a consumer needs wired when one connection url is not enough (s3:
// endpoint, bucket, keys). Derived from the outputs themselves so the two
// cannot drift.
func (s *Service) AutoInjectVars(ctx context.Context, instance *repo.Tile, p *repo.Provision) map[string]string {
	vars := map[string]string{}
	for _, o := range s.outputsFor(ctx, instance, p) {
		vars[o.Name] = Ref(instance, p, o.Name)
	}
	return vars
}

// unbindResource revokes one consumer's access without touching the slice,
// other consumers of the same database keep working.
func (s *Service) unbindResource(ctx context.Context, instance *repo.Tile, p *repo.Provision) {
	res, err := s.findResource(ctx, instance, p)
	if err != nil || res == nil {
		return
	}
	if err := s.store.DeleteBinding(ctx, res.ID, p.ConsumerTileID); err != nil {
		slog.Error("resource binding not deleted", "resource", res.ID, "tile", p.ConsumerTileID, "error", err)
	}
	s.dropConsumerRefs(ctx, res, p.ConsumerTileID)
}

// dropConsumerRefs removes the consumer's variables that point at a resource it
// may no longer read. Without this a detach bricks the tile: an unbound
// reference is a hard resolve error, so every later deploy fails until someone
// hand-edits the variable.
func (s *Service) dropConsumerRefs(ctx context.Context, res *repo.ManagedResource, consumerTileID string) {
	t, err := s.store.GetTile(ctx, consumerTileID)
	if err != nil || t == nil {
		return
	}
	needle := "tile." + res.Slug + "."
	var kept []string
	var dropped []string
	for _, v := range envutil.Parse(t.Env) {
		if strings.Contains(v.Value, needle) {
			dropped = append(dropped, v.Key)
			continue
		}
		kept = append(kept, v.Key+"="+v.Value)
	}
	if len(dropped) == 0 {
		return
	}
	t.Env = strings.Join(kept, "\n")
	if err := s.store.UpdateTile(ctx, t); err != nil {
		return
	}
	// The blob projection only ever adds, so the rows have to go explicitly.
	for _, name := range dropped {
		if err := s.store.DeleteVariable(ctx, repo.OwnerTile, t.ID, name); err != nil {
			slog.Error("dropped reference variable not deleted", "tile", t.ID, "name", name, "error", err)
		}
	}
}

// dropResource removes the slice itself, taking its outputs and every
// consumer's binding with it.
//
// Each consumer's *references* go too, for the reason dropConsumerRefs
// exists: a reference to a resource that is gone is a hard resolve error, so
// leaving them behind means every later deploy of those tiles fails until
// someone hand-edits the variable. Detach already did this for the one
// consumer it unhooks; dropping the slice unhooks all of them, and used to
// leave every one of them broken.
func (s *Service) dropResource(ctx context.Context, instance *repo.Tile, p *repo.Provision) {
	res, err := s.findResource(ctx, instance, p)
	if err != nil || res == nil {
		return
	}
	for _, id := range s.consumersOf(ctx, instance, p) {
		s.dropConsumerRefs(ctx, res, id)
	}
	if err := s.store.DeleteResource(ctx, res.ID); err != nil {
		slog.Error("resource row not deleted", "resource", res.ID, "error", err)
	}
}

// consumersOf lists the tiles bound to a slice, every provision row naming
// the same database on the same instance, which is the set DropDB removes.
func (s *Service) consumersOf(ctx context.Context, instance *repo.Tile, p *repo.Provision) []string {
	ps, err := s.store.ListProvisionsByInstance(ctx, instance.ID)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for i := range ps {
		id := ps[i].ConsumerTileID
		if ps[i].DBName != p.DBName || id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (s *Service) findResource(ctx context.Context, instance *repo.Tile, p *repo.Provision) (*repo.ManagedResource, error) {
	list, err := s.store.ListResourcesByEnv(ctx, p.EnvID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ProviderTileID == instance.ID && list[i].Name == p.DBName {
			return &list[i], nil
		}
	}
	return nil, nil
}
