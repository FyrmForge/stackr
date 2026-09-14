package sqlite

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Decrypt errors are returned, never swallowed: a variable that fails to
// decrypt would otherwise reach a container as ciphertext, which the resolver
// can't tell apart from a legitimate value.

func (s *Store) ListVariables(ctx context.Context, ownerKind, ownerID string) ([]repo.Variable, error) {
	out, err := list[repo.Variable](ctx, s,
		`SELECT * FROM variables WHERE owner_kind = ? AND owner_id = ? ORDER BY name`, ownerKind, ownerID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Value, err = secrets.Decrypt(out[i].Value); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListVariableNames feeds global search: owner + name + secret flag only.
// value is deliberately not selected, so nothing is decrypted and no secret
// material ever flows through the search path.
func (s *Store) ListVariableNames(ctx context.Context) ([]repo.Variable, error) {
	return list[repo.Variable](ctx, s,
		`SELECT owner_kind, owner_id, name, secret, created_at, updated_at FROM variables ORDER BY name`)
}

// UpsertVariable overwrites by (owner, name), same name twice is one row, not
// the key_2 suffixing the old env blob did.
func (s *Store) UpsertVariable(ctx context.Context, v *repo.Variable) error {
	enc := *v
	enc.Value = secrets.Encrypt(v.Value)
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO variables (owner_kind, owner_id, name, value, secret, created_at, updated_at)
		 VALUES (:owner_kind, :owner_id, :name, :value, :secret, :created_at, :updated_at)
		 ON CONFLICT (owner_kind, owner_id, name) DO UPDATE SET
		   value = excluded.value, secret = excluded.secret, updated_at = excluded.updated_at`, &enc)
	return err
}

func (s *Store) DeleteVariable(ctx context.Context, ownerKind, ownerID, name string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM variables WHERE owner_kind = ? AND owner_id = ? AND name = ?`, ownerKind, ownerID, name)
	return err
}

func (s *Store) CreateResource(ctx context.Context, r *repo.ManagedResource) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO managed_resources (id, environment_id, provider_tile_id, name, slug, kind, status, public, created_at, updated_at)
		 VALUES (:id, :environment_id, :provider_tile_id, :name, :slug, :kind, :status, :public, :created_at, :updated_at)`, r)
	return err
}

func (s *Store) GetResource(ctx context.Context, id string) (*repo.ManagedResource, error) {
	return get[repo.ManagedResource](ctx, s, `SELECT * FROM managed_resources WHERE id = ?`, id)
}

func (s *Store) ListResourcesByEnv(ctx context.Context, envID string) ([]repo.ManagedResource, error) {
	return list[repo.ManagedResource](ctx, s,
		`SELECT * FROM managed_resources WHERE environment_id = ? ORDER BY slug`, envID)
}

func (s *Store) ListResourcesByProvider(ctx context.Context, providerTileID string) ([]repo.ManagedResource, error) {
	return list[repo.ManagedResource](ctx, s,
		`SELECT * FROM managed_resources WHERE provider_tile_id = ? ORDER BY slug`, providerTileID)
}

func (s *Store) UpdateResource(ctx context.Context, r *repo.ManagedResource) error {
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE managed_resources SET name = :name, slug = :slug, kind = :kind, status = :status,
		 public = :public, provider_tile_id = :provider_tile_id, updated_at = :updated_at
		 WHERE id = :id`, r)
	return err
}

// DeleteResource takes its outputs and bindings with it, SQLite runs without
// foreign keys here, so nothing else would collect the orphans.
func (s *Store) DeleteResource(ctx context.Context, id string) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM resource_outputs WHERE resource_id = ?`,
		`DELETE FROM resource_bindings WHERE resource_id = ?`,
		`DELETE FROM managed_resources WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListOutputs(ctx context.Context, resourceID string) ([]repo.ResourceOutput, error) {
	out, err := list[repo.ResourceOutput](ctx, s,
		`SELECT * FROM resource_outputs WHERE resource_id = ? ORDER BY name`, resourceID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Value, err = secrets.Decrypt(out[i].Value); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) UpsertOutput(ctx context.Context, o *repo.ResourceOutput) error {
	enc := *o
	enc.Value = secrets.Encrypt(o.Value)
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO resource_outputs (resource_id, name, value, secret, requires_network)
		 VALUES (:resource_id, :name, :value, :secret, :requires_network)
		 ON CONFLICT (resource_id, name) DO UPDATE SET
		   value = excluded.value, secret = excluded.secret, requires_network = excluded.requires_network`, &enc)
	return err
}

func (s *Store) CreateBinding(ctx context.Context, b *repo.ResourceBinding) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO resource_bindings (resource_id, consumer_tile_id, created_at)
		 VALUES (:resource_id, :consumer_tile_id, :created_at)
		 ON CONFLICT (resource_id, consumer_tile_id) DO NOTHING`, b)
	return err
}

func (s *Store) DeleteBinding(ctx context.Context, resourceID, consumerTileID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM resource_bindings WHERE resource_id = ? AND consumer_tile_id = ?`, resourceID, consumerTileID)
	return err
}

func (s *Store) ListBindingsByResource(ctx context.Context, resourceID string) ([]repo.ResourceBinding, error) {
	return list[repo.ResourceBinding](ctx, s,
		`SELECT * FROM resource_bindings WHERE resource_id = ? ORDER BY created_at`, resourceID)
}

// BindingsForConsumer is the resolver's authorization lookup: everything this
// tile is allowed to read outputs from.
func (s *Store) BindingsForConsumer(ctx context.Context, consumerTileID string) ([]repo.ResourceBinding, error) {
	return list[repo.ResourceBinding](ctx, s,
		`SELECT * FROM resource_bindings WHERE consumer_tile_id = ? ORDER BY created_at`, consumerTileID)
}
