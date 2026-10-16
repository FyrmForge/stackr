package store

import (
	"context"
	"time"
)

// Node is a row of nodes: a machine stackr runs envs on. Tags is a JSON
// array of capabilities; RangeCIDR is the node's /16 of the cluster supernet.
type Node struct {
	ID          string    `db:"id" json:"id"`
	Name        string    `db:"name" json:"name"`
	Slug        string    `db:"slug" json:"slug"`
	Role        string    `db:"role" json:"role"` // controller | worker
	Tags        string    `db:"tags" json:"tags"`
	RangeCIDR   string    `db:"range_cidr" json:"range"`
	WGPublicKey string    `db:"wg_public_key" json:"wg_public_key"`
	WGAddress   string    `db:"wg_address" json:"wg_address"`
	Status      string    `db:"status" json:"status"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

type NodeStore interface {
	Create(ctx context.Context, n Node) error
	Get(ctx context.Context, id string) (Node, error)
	GetBySlug(ctx context.Context, slug string) (Node, error)
	// GetByRole is the first node of a role; the controller is the only one
	// of its role (a unique index).
	GetByRole(ctx context.Context, role string) (Node, error)
	List(ctx context.Context) ([]Node, error)
	Update(ctx context.Context, n Node) error
	Delete(ctx context.Context, id string) error
}

var nodesT = newTable[Node]("nodes", nil)

type nodes struct{ crud[Node] }

func (s nodes) GetBySlug(ctx context.Context, slug string) (Node, error) {
	return s.one(ctx, "slug = ?", slug)
}

func (s nodes) GetByRole(ctx context.Context, role string) (Node, error) {
	return s.one(ctx, "role = ? LIMIT 1", role)
}

func (s nodes) List(ctx context.Context) ([]Node, error) { return s.many(ctx, "1 = 1") }
