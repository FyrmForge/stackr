package sqlite

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func (s *Store) CreateProvision(ctx context.Context, p *repo.Provision) error {
	enc := *p
	enc.DBPassword = secrets.Encrypt(p.DBPassword)
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO provisions (id, instance_tile_id, consumer_tile_id, env_id, db_name, db_user, db_password, secret_name, status, public, on_remove, resource_slug, created_at)
		 VALUES (:id, :instance_tile_id, :consumer_tile_id, :env_id, :db_name, :db_user, :db_password, :secret_name, :status, :public, :on_remove, :resource_slug, :created_at)`, &enc)
	return err
}

func decryptProvision(p *repo.Provision, err error) (*repo.Provision, error) {
	if err != nil || p == nil {
		return p, err
	}
	p.DBPassword, err = secrets.Decrypt(p.DBPassword)
	return p, err
}

func (s *Store) GetProvision(ctx context.Context, id string) (*repo.Provision, error) {
	return decryptProvision(get[repo.Provision](ctx, s, `SELECT * FROM provisions WHERE id = ?`, id))
}

func (s *Store) listProvisions(ctx context.Context, q, arg string) ([]repo.Provision, error) {
	out, err := list[repo.Provision](ctx, s, q, arg)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].DBPassword, err = secrets.Decrypt(out[i].DBPassword); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) ListProvisionsByInstance(ctx context.Context, instanceTileID string) ([]repo.Provision, error) {
	return s.listProvisions(ctx, `SELECT * FROM provisions WHERE instance_tile_id = ? ORDER BY db_name`, instanceTileID)
}

func (s *Store) ListProvisionsByConsumer(ctx context.Context, consumerTileID string) ([]repo.Provision, error) {
	return s.listProvisions(ctx, `SELECT * FROM provisions WHERE consumer_tile_id = ? ORDER BY db_name`, consumerTileID)
}

func (s *Store) ListProvisionsByEnv(ctx context.Context, envID string) ([]repo.Provision, error) {
	return s.listProvisions(ctx, `SELECT * FROM provisions WHERE env_id = ? ORDER BY db_name`, envID)
}

func (s *Store) UpdateProvision(ctx context.Context, p *repo.Provision) error {
	enc := *p
	enc.DBPassword = secrets.Encrypt(p.DBPassword)
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE provisions SET consumer_tile_id = :consumer_tile_id, env_id = :env_id, db_name = :db_name,
		 db_user = :db_user, db_password = :db_password, secret_name = :secret_name, status = :status,
		 public = :public, on_remove = :on_remove, resource_slug = :resource_slug
		 WHERE id = :id`, &enc)
	return err
}

func (s *Store) DeleteProvision(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM provisions WHERE id = ?`, id)
	return err
}
