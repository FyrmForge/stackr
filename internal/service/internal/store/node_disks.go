package store

import (
	"context"
	"time"
)

// NodeDisk is a row of node_disks: the one disk a node's volumes may live on.
// No row = no disk. State is adding, ready or missing (phase 2 adds more). The
// Op fields carry an unfinished adopt and are empty when none is pending.
type NodeDisk struct {
	ID           string    `db:"id" json:"id"`
	NodeID       string    `db:"node_id" json:"node_id"`
	Kind         string    `db:"kind" json:"kind"`
	MountPath    string    `db:"mount_path" json:"mount_path"`
	State        string    `db:"state" json:"state"`
	OpNodeID     string    `db:"op_node_id" json:"op_node_id"`
	OpFromNodeID string    `db:"op_from_node_id" json:"op_from_node_id"`
	OpMountPath  string    `db:"op_mount_path" json:"op_mount_path"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

type NodeDiskStore interface {
	Create(ctx context.Context, d NodeDisk) error
	Get(ctx context.Context, id string) (NodeDisk, error)
	// GetByNode is the node's disk; errs.ErrNotFound when it has none.
	GetByNode(ctx context.Context, nodeID string) (NodeDisk, error)
	List(ctx context.Context) ([]NodeDisk, error)
	// Update writes every column, matched by id.
	Update(ctx context.Context, d NodeDisk) error
	// Delete removes the disk and, by cascade, its folder rows.
	Delete(ctx context.Context, id string) error
}

var nodeDisksT = newTable[NodeDisk]("node_disks", nil)

type nodeDisks struct{ crud[NodeDisk] }

func (s nodeDisks) GetByNode(ctx context.Context, nodeID string) (NodeDisk, error) {
	return s.one(ctx, "node_id = ?", nodeID)
}

func (s nodeDisks) List(ctx context.Context) ([]NodeDisk, error) {
	return s.many(ctx, "1 = 1 ORDER BY created_at, id")
}
