package managedtiles

import (
	"context"
	"fmt"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/google/uuid"
)

// CanProvision reports whether the engine supports provisioning a per-consumer
// slice (a logical db for postgres, a bucket for s3), i.e. whether its
// registry entry carries a Provision hook.
func CanProvision(engine string) bool { return Engines[engine].Provision != nil }

// sliceSecret is the provision's secret name. vestigial field, kept
// until the legacy-drop migration; the per-engine suffix preserves the names
// already stored.
func sliceSecret(instance, consumer *repo.Tile) string {
	return instance.Slug + "_" + consumer.Slug + Engines[instance.Engine].SecretSuffix
}

// Provision cuts a per-consumer slice out of the shared instance, a logical
// database for postgres, a bucket for s3, records it, and publishes its
// connection details as resource outputs.
// name is the slice name; "" derives it from the consumer slug. It is
// uniquified against the instance's existing slices. public asks for
// unauthenticated read access, which only engines with PublicSlices support.
func (s *Service) Provision(ctx context.Context, instance, consumer *repo.Tile, name string, public bool) (*repo.Provision, error) {
	eng := Engines[instance.Engine]
	if eng.Provision == nil {
		return nil, fmt.Errorf("provisioning not supported for engine %q yet", instance.Engine)
	}
	if public && !eng.PublicSlices {
		return nil, fmt.Errorf("%s slices cannot be made public", instance.Engine)
	}
	return eng.Provision(s, ctx, instance, consumer, name, public)
}

// ProvisionSlice cuts (or adopts) a config-declared slice: slug is its
// reference name (the config key), name the db/bucket name, envID where it
// lives. No consumer on the row, consumers are whichever tiles reference it,
// bound at apply time.
//
// An existing slice on the instance with the same name and env is adopted
// (re-stamped with the slug, revived if orphaned) rather than uniquified into
// a silent second copy: the file names the slice it means.
func (s *Service) ProvisionSlice(ctx context.Context, instance *repo.Tile, envID, slug, name string, public bool) (*repo.Provision, error) {
	existing, err := s.store.ListProvisionsByInstance(ctx, instance.ID)
	if err != nil {
		return nil, err
	}
	for i := range existing {
		p := &existing[i]
		if p.DBName != name {
			continue
		}
		if p.EnvID != envID {
			return nil, fmt.Errorf("%s %q already exists on %s in another environment", sliceNoun(instance.Engine), name, instance.Slug)
		}
		p.ResourceSlug = slug
		p.Status = "active"
		if err := s.rows.UpdateProvision(ctx, p); err != nil {
			return nil, err
		}
		if public != p.Public && Engines[instance.Engine].PublicSlices {
			if err := s.SetBucketPublic(ctx, instance, p, public); err != nil {
				return nil, err
			}
		}
		return p, s.SyncResource(ctx, instance, p)
	}
	// The synthetic consumer carries only what the hooks read: the env the
	// slice lands in. ConsumerTileID stays "", no binding.
	p, err := s.Provision(ctx, instance, &repo.Tile{Slug: name, EnvironmentID: envID}, name, public)
	if err != nil {
		return nil, err
	}
	p.ResourceSlug = slug
	if err := s.rows.UpdateProvision(ctx, p); err != nil {
		return nil, err
	}
	// Re-sync renames the hook-created resource onto the config key.
	return p, s.SyncResource(ctx, instance, p)
}

// CloneSlice cuts a fresh slice for a cloned env under the same reference
// slug, a new database/bucket (name uniquified against the instance), never
// a second consumer of the base env's data.
func (s *Service) CloneSlice(ctx context.Context, instance *repo.Tile, envID, slug, name string, public bool) (*repo.Provision, error) {
	p, err := s.Provision(ctx, instance, &repo.Tile{Slug: name, EnvironmentID: envID}, name, public)
	if err != nil {
		return nil, err
	}
	p.ResourceSlug = slug
	if err := s.rows.UpdateProvision(ctx, p); err != nil {
		return nil, err
	}
	return p, s.SyncResource(ctx, instance, p)
}

// AttachExisting points a second consumer at an existing logical db (same
// creds), so cron jobs and services can share one database. Same env only,
// the url secret is env-scoped. No CREATE DATABASE; just a new consumer row +
// its own url secret. The consumer joins the shared net on its next deploy.
func (s *Service) AttachExisting(ctx context.Context, instance *repo.Tile, src *repo.Provision, consumer *repo.Tile) (*repo.Provision, error) {
	if src.EnvID != consumer.EnvironmentID {
		return nil, fmt.Errorf("can only attach to databases in the same environment")
	}
	// Keep the shared network + instance alias current (idempotent). Engine-
	// neutral: attaching creates no slice, so there is nothing engine-specific
	// left to do beyond the row.
	_ = s.joinSharedNet(ctx, instance)
	p := &repo.Provision{
		ID:             uuid.NewString(),
		InstanceTileID: instance.ID,
		ConsumerTileID: consumer.ID,
		EnvID:          consumer.EnvironmentID,
		DBName:         src.DBName,
		DBUser:         src.DBUser,
		DBPassword:     src.DBPassword,
		SecretName:     sliceSecret(instance, consumer),
		Status:         "active",
		Public:         src.Public, // shares the source slice's visibility
		CreatedAt:      time.Now().UTC(),
	}
	if err := s.rows.RecordProvision(ctx, p); err != nil {
		return nil, err
	}
	if err := s.SyncResource(ctx, instance, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Detach unlinks the consumer but keeps the logical db (orphaned), revoking
// the binding so its references stop resolving.
func (s *Service) Detach(ctx context.Context, p *repo.Provision) error {
	if inst, err := s.store.GetTile(ctx, p.InstanceTileID); err == nil && inst != nil {
		s.unbindResource(ctx, inst, p)
	}
	p.ConsumerTileID = ""
	p.Status = "orphaned"
	return s.rows.UpdateProvision(ctx, p)
}

// DropDB destroys a provisioned slice (logical database, bucket) and removes
// every consumer's provision row with it. This is the whole-slice teardown
// from the instance panel; per-consumer removal is Detach on the consumer
// side.
func (s *Service) DropDB(ctx context.Context, instance *repo.Tile, dbName string) error {
	ps, err := s.store.ListProvisionsByInstance(ctx, instance.ID)
	if err != nil {
		return err
	}
	var rows []repo.Provision
	for i := range ps {
		if ps[i].DBName == dbName {
			rows = append(rows, ps[i])
		}
	}
	if len(rows) == 0 {
		return fmt.Errorf("no provisioned %s %q", sliceNoun(instance.Engine), dbName)
	}
	drop := Engines[instance.Engine].Drop
	if drop == nil {
		return fmt.Errorf("engine %q cannot drop slices", instance.Engine)
	}
	// The rows share one slice; dropping it once is enough.
	if err := drop(s, ctx, instance, &rows[0]); err != nil {
		return err
	}
	for i := range rows {
		s.dropResource(ctx, instance, &rows[i])
		if err := s.rows.RemoveProvision(ctx, rows[i].ID); err != nil {
			return err
		}
	}
	return nil
}

// SharedDB identifies one logical database available to attach to (deduped
// across its consumers).
type SharedDB struct {
	Provision repo.Provision // a representative row (creds shared across consumers)
	Instance  repo.Tile
	Consumers []string // consumer tile display names already using it
}

// AttachableDBs lists logical dbs in the consumer's env that it may attach to
// but isn't already consuming, for the "attach to existing" picker.
func AttachableDBs(ctx context.Context, store repo.Store, consumer *repo.Tile) ([]SharedDB, error) {
	ps, err := store.ListProvisionsByEnv(ctx, consumer.EnvironmentID)
	if err != nil {
		return nil, err
	}
	mine := map[string]bool{} // instance|db already consumed by this tile
	byDB := map[string]*SharedDB{}
	var order []string
	for i := range ps {
		key := ps[i].InstanceTileID + "|" + ps[i].DBName
		if ps[i].ConsumerTileID == consumer.ID {
			mine[key] = true
		}
		sd := byDB[key]
		if sd == nil {
			inst, _ := store.GetTile(ctx, ps[i].InstanceTileID)
			if inst == nil {
				continue
			}
			sd = &SharedDB{Provision: ps[i], Instance: *inst}
			byDB[key] = sd
			order = append(order, key)
		}
		if ps[i].ConsumerTileID != "" {
			if t, _ := store.GetTile(ctx, ps[i].ConsumerTileID); t != nil {
				sd.Consumers = append(sd.Consumers, t.Name)
			}
		}
	}
	out := make([]SharedDB, 0, len(order))
	for _, key := range order {
		if mine[key] {
			continue
		}
		out = append(out, *byDB[key])
	}
	return out, nil
}
