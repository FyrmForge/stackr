package store

import (
	"context"
	"time"
)

// HostGrant is a row of host_grants: the host access a server admin approved
// for a stack. Lines is the approved host mount lines, sorted, newline-joined.
type HostGrant struct {
	ID         string    `db:"id" json:"id"`
	StackID    string    `db:"stack_id" json:"stack_id"`
	Lines      string    `db:"lines" json:"lines"`
	Privileged bool      `db:"privileged" json:"privileged"`
	ApprovedBy string    `db:"approved_by" json:"approved_by"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

type HostGrantStore interface {
	Create(ctx context.Context, g HostGrant) error
	Get(ctx context.Context, id string) (HostGrant, error)
	GetByStack(ctx context.Context, stackID string) (HostGrant, error)
	Update(ctx context.Context, g HostGrant) error
	Delete(ctx context.Context, id string) error
}

var hostGrantsT = newTable[HostGrant]("host_grants", nil)

type hostGrants struct{ crud[HostGrant] }

func (s hostGrants) GetByStack(ctx context.Context, stackID string) (HostGrant, error) {
	return s.one(ctx, "stack_id = ?", stackID)
}
