package store

import (
	"context"
	"time"
)

// DiskFolder is a row of disk_folders: the folder <mount>/<disk id>/<Name> on
// a disk. Name is the Docker volume name; (DiskID, Name) is unique.
type DiskFolder struct {
	ID        string    `db:"id" json:"id"`
	DiskID    string    `db:"disk_id" json:"disk_id"`
	Name      string    `db:"name" json:"name"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type DiskFolderStore interface {
	Create(ctx context.Context, f DiskFolder) error
	ListByDisk(ctx context.Context, diskID string) ([]DiskFolder, error)
	Delete(ctx context.Context, id string) error
}

var diskFoldersT = newTable[DiskFolder]("disk_folders", nil)

type diskFolders struct{ crud[DiskFolder] }

func (s diskFolders) ListByDisk(ctx context.Context, diskID string) ([]DiskFolder, error) {
	return s.many(ctx, "disk_id = ? ORDER BY name", diskID)
}
