package store

import (
	"context"
	"time"
)

// BackupGroup is a row of backup_groups: one node-wide backup. NodeID has no
// foreign key, a group outlives a removed node. Reason is manual or remove;
// Warning is "" or the local-fallback warning.
type BackupGroup struct {
	ID         string     `db:"id" json:"id"`
	NodeID     string     `db:"node_id" json:"node_id"`
	NodeName   string     `db:"node_name" json:"node_name"`
	Label      string     `db:"label" json:"label"`
	Reason     string     `db:"reason" json:"reason"`
	DestID     string     `db:"dest_id" json:"dest_id"`
	Warning    string     `db:"warning" json:"warning"`
	Status     string     `db:"status" json:"status"`
	Error      string     `db:"error" json:"error"`
	CreatedAt  time.Time  `db:"created_at" json:"created_at"`
	FinishedAt *time.Time `db:"finished_at" json:"finished_at"`
}

type BackupGroupStore interface {
	Create(ctx context.Context, g BackupGroup) error
	Get(ctx context.Context, id string) (BackupGroup, error)
	ListByNode(ctx context.Context, nodeID string) ([]BackupGroup, error)
	Update(ctx context.Context, g BackupGroup) error
}

var backupGroupsT = newTable[BackupGroup]("backup_groups", nil)

type backupGroups struct{ crud[BackupGroup] }

func (s backupGroups) ListByNode(ctx context.Context, nodeID string) ([]BackupGroup, error) {
	return s.many(ctx, "node_id = ?", nodeID)
}
