package sqlite

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func (s *Store) CreateCronRun(ctx context.Context, r *repo.CronRun) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO cron_runs (id, ref, status, trigger, actor, output, started_at, finished_at)
		 VALUES (:id, :ref, :status, :trigger, :actor, :output, :started_at, :finished_at)`, r)
	return err
}

// CloseOrphanCronRuns finishes runs left open by a crash or a restart. The row
// is written before the work starts, and the in-memory overlap lock does not
// survive the process, so without this the panel shows a run as still going
// forever, and nothing will ever close it.
func (s *Store) CloseOrphanCronRuns(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE cron_runs SET status = 'error', finished_at = ?,
		 output = CASE WHEN output = '' THEN 'interrupted: stackr restarted while this run was in progress' ELSE output END
		 WHERE finished_at IS NULL`, time.Now().UTC())
	return err
}

// FinishCronRun closes out a run inserted while it was still going. Only the
// three columns a finish can move are written, so nothing races the insert.
func (s *Store) FinishCronRun(ctx context.Context, r *repo.CronRun) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE cron_runs SET status = ?, output = ?, finished_at = ? WHERE id = ?`,
		r.Status, r.Output, r.FinishedAt, r.ID)
	return err
}

// rowid rather than started_at (TEXT, format-sensitive), see LatestConfigPlan
// in stacks.go. PruneCronRuns below is fine as is: it compares against a bound
// time.Time, so the driver normalises both sides to one format.
func (s *Store) ListCronRuns(ctx context.Context, ref string, limit int) ([]repo.CronRun, error) {
	return list[repo.CronRun](ctx, s,
		`SELECT * FROM cron_runs WHERE ref = ? ORDER BY rowid DESC LIMIT ?`, ref, limit)
}

func (s *Store) GetCronRun(ctx context.Context, id string) (*repo.CronRun, error) {
	return get[repo.CronRun](ctx, s, `SELECT * FROM cron_runs WHERE id = ?`, id)
}

// OpenCronRun is the newest run of ref that has not finished; nil when there
// is none. The panel header asks this to know whether a tile is mid-run,
// tiles.status never says "running" for a cron.
func (s *Store) OpenCronRun(ctx context.Context, ref string) (*repo.CronRun, error) {
	return get[repo.CronRun](ctx, s,
		`SELECT * FROM cron_runs WHERE ref = ? AND finished_at IS NULL ORDER BY rowid DESC LIMIT 1`, ref)
}

// ListOpenCronRuns is every run in flight, for the canvas: one query instead
// of one per cron card.
func (s *Store) ListOpenCronRuns(ctx context.Context) ([]repo.CronRun, error) {
	return list[repo.CronRun](ctx, s, `SELECT * FROM cron_runs WHERE finished_at IS NULL ORDER BY rowid DESC`)
}

func (s *Store) PruneCronRuns(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cron_runs WHERE started_at < ?`, before.UTC())
	return err
}
