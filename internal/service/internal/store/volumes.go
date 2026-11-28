package store

import (
	"context"
	"time"
)

// Volume is a row of volumes. InstanceID is set when an operated instance
// owns it; OrphanedAt when its owner is gone and the retention clock runs.
type Volume struct {
	ID         string     `db:"id" json:"id"`
	ScopeKind  string     `db:"scope_kind" json:"scope_kind"`
	ScopeID    string     `db:"scope_id" json:"scope_id"`
	InstanceID *string    `db:"instance_id" json:"instance_id"`
	Slug       string     `db:"slug" json:"slug"`
	Name       string     `db:"name" json:"name"`
	OrphanedAt *time.Time `db:"orphaned_at" json:"orphaned_at"`
	CreatedAt  time.Time  `db:"created_at" json:"created_at"`
	// NodeID is where an orphan's data lives once its env row is gone; ''
	// means the controller. Set by the env delete trigger.
	NodeID string `db:"node_id" json:"node_id"`
	// MaxSizeMB is the volume's size cap in MB; nil = uncapped.
	MaxSizeMB *int64 `db:"max_size_mb" json:"max_size_mb"`
	// FromInstance is set when an operated instance owned the volume (stamped
	// at orphaning, while InstanceID is still set; the FK clears it later).
	FromInstance bool `db:"from_instance" json:"from_instance"`
}

type VolumeStore interface {
	Create(ctx context.Context, v Volume) error
	Get(ctx context.Context, id string) (Volume, error)
	ListByScope(ctx context.Context, scopeKind, scopeID string) ([]Volume, error)
	ListOrphaned(ctx context.Context) ([]Volume, error)
	// ListLive lists every volume that is not an orphan.
	ListLive(ctx context.Context) ([]Volume, error)
	// ListByNode lists the orphans pinned to nodeID.
	ListByNode(ctx context.Context, nodeID string) ([]Volume, error)
	Update(ctx context.Context, v Volume) error
	Delete(ctx context.Context, id string) error
	// DeleteIfOrphaned deletes the row only while it is still orphaned since at;
	// false when a restore (or a re-orphan) got there first.
	DeleteIfOrphaned(ctx context.Context, id string, at time.Time) (bool, error)
	// RepointNode moves every volume pinned to node from onto node to (an
	// adopt hands a dead node's orphans to the adopting node). Idempotent.
	RepointNode(ctx context.Context, from, to string) error
}

var volumesT = newTable[Volume]("volumes", nil)

type volumes struct{ crud[Volume] }

func (s volumes) ListByScope(ctx context.Context, scopeKind, scopeID string) ([]Volume, error) {
	return s.many(ctx, "scope_kind = ? AND scope_id = ?", scopeKind, scopeID)
}

func (s volumes) ListOrphaned(ctx context.Context) ([]Volume, error) {
	return s.many(ctx, "orphaned_at IS NOT NULL")
}

func (s volumes) ListLive(ctx context.Context) ([]Volume, error) {
	return s.many(ctx, "orphaned_at IS NULL")
}

func (s volumes) ListByNode(ctx context.Context, nodeID string) ([]Volume, error) {
	return s.many(ctx, "orphaned_at IS NOT NULL AND node_id = ?", nodeID)
}

func (s volumes) RepointNode(ctx context.Context, from, to string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE volumes SET node_id = ? WHERE node_id = ? AND node_id != ''`, to, from)
	return mapErr(err)
}

func (s volumes) DeleteIfOrphaned(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := s.q.ExecContext(ctx, `DELETE FROM volumes WHERE id = ? AND orphaned_at = ?`, id, at)
	if err != nil {
		return false, mapErr(err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
