package store

import (
	"context"
	"time"
)

// OperatedMember is a row of operated_members: one container of an operated
// instance with its own data volume and node. The primary's NodeID, Container
// and Volume are empty: they follow the instance tile (and so its env).
type OperatedMember struct {
	ID         string `db:"id" json:"id"`
	InstanceID string `db:"instance_id" json:"instance_id"`
	Role       string `db:"role" json:"role"`
	NodeID     string `db:"node_id" json:"node_id"`
	Container  string `db:"container" json:"container"`
	Volume     string `db:"volume" json:"volume"`
	// Subnet is the member's network on its node, the source of its
	// cross-node firewall flow.
	Subnet string `db:"subnet" json:"subnet"`
	// State is what the panel wants: running members are started by the
	// panel (their restart policy is "no"), stopped ones are left alone.
	State string `db:"state" json:"state"`
	// Fenced members are never started, whatever State says.
	Fenced bool `db:"fenced" json:"fenced"`
	// Phase is what the panel observes of a replica: starting, streaming or
	// rebuild (see leaf/opmember).
	Phase     string    `db:"phase" json:"phase"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

type OperatedMemberStore interface {
	Create(ctx context.Context, m OperatedMember) error
	Get(ctx context.Context, id string) (OperatedMember, error)
	ListByInstance(ctx context.Context, instanceID string) ([]OperatedMember, error)
	// List is every member of every instance.
	List(ctx context.Context) ([]OperatedMember, error)
	Update(ctx context.Context, m OperatedMember) error
	Delete(ctx context.Context, id string) error
}

var operatedMembersT = newTable[OperatedMember]("operated_members", nil)

type operatedMembers struct{ crud[OperatedMember] }

func (s operatedMembers) ListByInstance(ctx context.Context, instanceID string) ([]OperatedMember, error) {
	return s.many(ctx, "instance_id = ?", instanceID)
}

func (s operatedMembers) List(ctx context.Context) ([]OperatedMember, error) {
	return s.many(ctx, "1 = 1")
}
