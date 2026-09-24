package store

import (
	"context"
	"time"
)

// Run is a row of runs: one run of a cron or function tile.
type Run struct {
	ID         string     `db:"id" json:"id"`
	TileID     string     `db:"tile_id" json:"tile_id"`
	ReleaseID  *string    `db:"release_id" json:"release_id"`
	JobID      string     `db:"job_id" json:"job_id"`
	Trigger    string     `db:"trigger" json:"trigger"`
	Status     string     `db:"status" json:"status"`
	ExitCode   *int       `db:"exit_code" json:"exit_code"`
	Reason     string     `db:"reason" json:"reason"`
	CreatedAt  time.Time  `db:"created_at" json:"created_at"`
	StartedAt  *time.Time `db:"started_at" json:"started_at"`
	FinishedAt *time.Time `db:"finished_at" json:"finished_at"`
}

type RunStore interface {
	Create(ctx context.Context, r Run) error
	Get(ctx context.Context, id string) (Run, error)
	ListByTile(ctx context.Context, tileID string) ([]Run, error)
	ListByStatus(ctx context.Context, status string) ([]Run, error)
	Update(ctx context.Context, r Run) error
	Delete(ctx context.Context, id string) error
}

var runsT = newTable[Run]("runs", nil)

type runs struct{ crud[Run] }

func (s runs) ListByTile(ctx context.Context, tileID string) ([]Run, error) {
	return s.many(ctx, "tile_id = ?", tileID)
}

func (s runs) ListByStatus(ctx context.Context, status string) ([]Run, error) {
	return s.many(ctx, "status = ?", status)
}
