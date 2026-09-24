package store

import (
	"context"
	"time"
)

// Volume is a row of volumes. InstanceID is set when a managed instance
// owns it; OrphanedAt when its owner is gone and the retention clock runs.
type Volume struct {
	ID         string     `db:"id" json:"id"`
	ScopeKind  string     `db:"scope_kind" json:"scope_kind"`
	ScopeID    string     `db:"scope_id" json:"scope_id"`
	InstanceID *string    `db:"instance_id" json:"instance_id"`
	Slug       string     `db:"slug" json:"slug"`
	Name       string     `db:"name" json:"name"`
	MaxSizeMB  int        `db:"max_size_mb" json:"max_size_mb"`
	OrphanedAt *time.Time `db:"orphaned_at" json:"orphaned_at"`
	CreatedAt  time.Time  `db:"created_at" json:"created_at"`
}

type VolumeStore interface {
	Create(ctx context.Context, v Volume) error
	Get(ctx context.Context, id string) (Volume, error)
	ListByScope(ctx context.Context, scopeKind, scopeID string) ([]Volume, error)
	ListOrphaned(ctx context.Context) ([]Volume, error)
	Update(ctx context.Context, v Volume) error
	Delete(ctx context.Context, id string) error
}

var volumesT = newTable[Volume]("volumes", nil)

type volumes struct{ crud[Volume] }

func (s volumes) ListByScope(ctx context.Context, scopeKind, scopeID string) ([]Volume, error) {
	return s.many(ctx, "scope_kind = ? AND scope_id = ?", scopeKind, scopeID)
}

func (s volumes) ListOrphaned(ctx context.Context) ([]Volume, error) {
	return s.many(ctx, "orphaned_at IS NOT NULL")
}
