package store

import (
	"context"
	"time"
)

// HostGrant is a row of host_grants: the host access a server admin approved
// for a stack. Lines is the approved "<tile-slug> <perm>" lines, sorted,
// newline-joined.
type HostGrant struct {
	ID         string    `db:"id" json:"id"`
	StackID    string    `db:"stack_id" json:"stack_id"`
	Lines      string    `db:"lines" json:"lines"`
	ApprovedBy string    `db:"approved_by" json:"approved_by"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

type HostGrantStore interface {
	Create(ctx context.Context, g HostGrant) error
	Get(ctx context.Context, id string) (HostGrant, error)
	GetByStack(ctx context.Context, stackID string) (HostGrant, error)
	Update(ctx context.Context, g HostGrant) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) ([]HostGrant, error)
}

var hostGrantsT = newTable[HostGrant]("host_grants", nil)

type hostGrants struct{ crud[HostGrant] }

func (s hostGrants) GetByStack(ctx context.Context, stackID string) (HostGrant, error) {
	return s.one(ctx, "stack_id = ?", stackID)
}

func (s hostGrants) List(ctx context.Context) ([]HostGrant, error) { return s.many(ctx, "1 = 1") }
