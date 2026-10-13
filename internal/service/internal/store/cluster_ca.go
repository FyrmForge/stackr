package store

import (
	"context"
	"errors"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// ClusterCA is the one row of cluster_ca: the CA's state outside its key
// files. PreviousFP is "" outside a rollover. A rollover is running while
// RolloverStartedAt is set and RolloverFinishedAt is nil or older.
type ClusterCA struct {
	CurrentFP          string     `db:"current_fp" json:"current_fp"`
	PreviousFP         string     `db:"previous_fp" json:"previous_fp"`
	RolloverStartedAt  *time.Time `db:"rollover_started_at" json:"rollover_started_at"`
	RolloverFinishedAt *time.Time `db:"rollover_finished_at" json:"rollover_finished_at"`
	BreakGlassAt       *time.Time `db:"break_glass_at" json:"break_glass_at"`
}

// RollingOver is whether a rollover has started and not finished.
func (c ClusterCA) RollingOver() bool {
	return c.RolloverStartedAt != nil &&
		(c.RolloverFinishedAt == nil || c.RolloverFinishedAt.Before(*c.RolloverStartedAt))
}

type ClusterCAStore interface {
	// Get answers (zero, false, nil) before the first Save.
	Get(ctx context.Context) (ClusterCA, bool, error)
	// Save writes the whole row (insert or replace).
	Save(ctx context.Context, c ClusterCA) error
}

// clusterCAs is the single-row table, so it does not use crud.
type clusterCAs struct{ q querier }

// NewClusterCA binds the store to a querier.
func NewClusterCA(q querier) ClusterCAStore { return clusterCAs{q} }

const clusterCACols = "current_fp, previous_fp, rollover_started_at, rollover_finished_at, break_glass_at"

func (s clusterCAs) Get(ctx context.Context) (ClusterCA, bool, error) {
	var c ClusterCA
	err := mapErr(s.q.GetContext(ctx, &c, "SELECT "+clusterCACols+" FROM cluster_ca WHERE id = 1"))
	if errors.Is(err, errs.ErrNotFound) {
		return ClusterCA{}, false, nil
	}
	return c, err == nil, err
}

func (s clusterCAs) Save(ctx context.Context, c ClusterCA) error {
	_, err := s.q.NamedExecContext(ctx, `INSERT INTO cluster_ca (id, `+clusterCACols+`)
VALUES (1, :current_fp, :previous_fp, :rollover_started_at, :rollover_finished_at, :break_glass_at)
ON CONFLICT (id) DO UPDATE SET current_fp = excluded.current_fp, previous_fp = excluded.previous_fp,
rollover_started_at = excluded.rollover_started_at, rollover_finished_at = excluded.rollover_finished_at,
break_glass_at = excluded.break_glass_at`, c)
	return mapErr(err)
}
