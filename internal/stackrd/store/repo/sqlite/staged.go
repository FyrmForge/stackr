package sqlite

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func (s *Store) CreateStagedChange(ctx context.Context, sc *repo.StagedChange) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO staged_changes (id, stack_id, env_id, tile_slug, author_id, author_name, summary, payload, created_at)
		 VALUES (:id, :stack_id, :env_id, :tile_slug, :author_id, :author_name, :summary, :payload, :created_at)`, sc)
	return err
}

// Oldest-first by rowid, i.e. insert order, see the note over
// LatestConfigPlan in stacks.go for why created_at (TEXT, format-sensitive)
// cannot order rows. This one is load-bearing, not just display: staging
// replays these in order and later entries win (stackconf/staging.go), so a
// mis-sorted row would apply a stale edit over a newer one.
func (s *Store) ListStagedByEnv(ctx context.Context, envID string) ([]repo.StagedChange, error) {
	return list[repo.StagedChange](ctx, s, `SELECT * FROM staged_changes WHERE env_id = ? ORDER BY rowid`, envID)
}

func (s *Store) ListStagedByStack(ctx context.Context, stackID string) ([]repo.StagedChange, error) {
	return list[repo.StagedChange](ctx, s, `SELECT * FROM staged_changes WHERE stack_id = ? ORDER BY rowid`, stackID)
}

func (s *Store) CountStagedByEnv(ctx context.Context, envID string) (int, error) {
	var n int
	err := s.db.GetContext(ctx, &n, `SELECT count(*) FROM staged_changes WHERE env_id = ?`, envID)
	return n, err
}

func (s *Store) GetStagedChange(ctx context.Context, id string) (*repo.StagedChange, error) {
	return get[repo.StagedChange](ctx, s, `SELECT * FROM staged_changes WHERE id = ?`, id)
}

func (s *Store) DeleteStagedChange(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM staged_changes WHERE id = ?`, id)
	return err
}

func (s *Store) DeleteStagedByEnv(ctx context.Context, envID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM staged_changes WHERE env_id = ?`, envID)
	return err
}
