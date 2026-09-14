package sqlite

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func (s *Store) ListIntended(ctx context.Context, envID string) ([]repo.Intended, error) {
	return list[repo.Intended](ctx, s,
		`SELECT * FROM env_intended WHERE environment_id = ? ORDER BY tile_slug, key`, envID)
}

func (s *Store) SetIntended(ctx context.Context, row *repo.Intended) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO env_intended (environment_id, tile_slug, key, value)
		 VALUES (:environment_id, :tile_slug, :key, :value)
		 ON CONFLICT (environment_id, tile_slug, key) DO UPDATE SET value = excluded.value`, row)
	return err
}

func (s *Store) DeleteIntended(ctx context.Context, envID, tileSlug, key string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM env_intended WHERE environment_id = ? AND tile_slug = ? AND key = ?`, envID, tileSlug, key)
	return err
}

func (s *Store) ClearDeclaredIntended(ctx context.Context, envID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM env_intended WHERE environment_id = ? AND value = ''`, envID)
	return err
}
