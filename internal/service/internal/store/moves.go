package store

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
)

// EnvMove is a row of env_moves. Phase is the step to do or in progress;
// done and rolled_back end the move. FromNode and ToNode are node ids.
type EnvMove struct {
	ID         string     `db:"id" json:"id"`
	EnvID      string     `db:"env_id" json:"env_id"`
	FromNode   string     `db:"from_node" json:"from_node"`
	ToNode     string     `db:"to_node" json:"to_node"`
	Live       bool       `db:"live" json:"live"`
	Phase      string     `db:"phase" json:"phase"`
	Bytes      int64      `db:"bytes" json:"bytes"`
	StartedAt  time.Time  `db:"started_at" json:"started_at"`
	FinishedAt *time.Time `db:"finished_at" json:"finished_at"`
	Error      string     `db:"error" json:"error"`
}

type MoveStore interface {
	Create(ctx context.Context, m EnvMove) error
	Get(ctx context.Context, id string) (EnvMove, error)
	// ListByEnv lists an env's moves, any order.
	ListByEnv(ctx context.Context, envID string) ([]EnvMove, error)
	// ListUnfinished lists the moves whose phase is neither done nor
	// rolled_back: the ones to resume after a restart.
	ListUnfinished(ctx context.Context) ([]EnvMove, error)
	Update(ctx context.Context, m EnvMove) error
	Delete(ctx context.Context, id string) error
}

var movesT = newTable[EnvMove]("env_moves", nil)

type moves struct{ crud[EnvMove] }

// NewMoves is the table on a plain handle (box may be nil: no secret
// columns), for the move flow's tests until Tables carries it.
func NewMoves(db *sqlx.DB, box *secrets.Box) MoveStore {
	return moves{crud[EnvMove]{db, box, movesT}}
}

func (s moves) ListByEnv(ctx context.Context, envID string) ([]EnvMove, error) {
	return s.many(ctx, "env_id = ?", envID)
}

func (s moves) ListUnfinished(ctx context.Context) ([]EnvMove, error) {
	return s.many(ctx, "phase NOT IN ('done', 'rolled_back')")
}
