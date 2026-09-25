package store

import (
	"context"
	"time"
)

// BackupDest is a row of backup_destinations. OrgID nil = admin-global.
type BackupDest struct {
	ID         string    `db:"id" json:"id"`
	OrgID      *string   `db:"org_id" json:"org_id"`
	Kind       string    `db:"kind" json:"kind"`
	Name       string    `db:"name" json:"name"`
	Endpoint   string    `db:"endpoint" json:"endpoint"`
	Region     string    `db:"region" json:"region"`
	Bucket     string    `db:"bucket" json:"bucket"`
	AccessKey  string    `db:"access_key" json:"-"`
	SecretKey  string    `db:"secret_key" json:"-"`
	ArchiveKey string    `db:"archive_key" json:"-"`
	Shared     bool      `db:"shared" json:"shared"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

type BackupDestStore interface {
	Create(ctx context.Context, d BackupDest) error
	Get(ctx context.Context, id string) (BackupDest, error)
	// ListByOrg lists an org's own destinations; nil lists the global ones.
	ListByOrg(ctx context.Context, orgID *string) ([]BackupDest, error)
	Update(ctx context.Context, d BackupDest) error
	Delete(ctx context.Context, id string) error
}

var backupDestsT = newTable("backup_destinations", func(d *BackupDest) []*string {
	return []*string{&d.SecretKey, &d.ArchiveKey}
})

type backupDests struct{ crud[BackupDest] }

func (s backupDests) ListByOrg(ctx context.Context, orgID *string) ([]BackupDest, error) {
	if orgID == nil {
		return s.many(ctx, "org_id IS NULL")
	}
	return s.many(ctx, "org_id = ?", *orgID)
}

// BackupSchedule is a row of backup_schedules. DestID nil = local default.
type BackupSchedule struct {
	ID        string    `db:"id" json:"id"`
	VolumeID  string    `db:"volume_id" json:"volume_id"`
	Method    string    `db:"method" json:"method"`
	DestID    *string   `db:"dest_id" json:"dest_id"`
	Cron      string    `db:"cron" json:"cron"`
	Timezone  string    `db:"timezone" json:"timezone"`
	Keep      int       `db:"keep" json:"keep"`
	Mode      string    `db:"mode" json:"mode"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type BackupScheduleStore interface {
	Create(ctx context.Context, b BackupSchedule) error
	Get(ctx context.Context, id string) (BackupSchedule, error)
	ListByVolume(ctx context.Context, volumeID string) ([]BackupSchedule, error)
	List(ctx context.Context) ([]BackupSchedule, error)
	Update(ctx context.Context, b BackupSchedule) error
	Delete(ctx context.Context, id string) error
}

var backupSchedulesT = newTable[BackupSchedule]("backup_schedules", nil)

type backupSchedules struct{ crud[BackupSchedule] }

func (s backupSchedules) ListByVolume(ctx context.Context, volumeID string) ([]BackupSchedule, error) {
	return s.many(ctx, "volume_id = ?", volumeID)
}

func (s backupSchedules) List(ctx context.Context) ([]BackupSchedule, error) {
	return s.many(ctx, "1 = 1")
}

// BackupRun is a row of backup_runs. VolumeID nil on a panel run or once the
// volume is gone.
type BackupRun struct {
	ID         string     `db:"id" json:"id"`
	Kind       string     `db:"kind" json:"kind"`
	VolumeID   *string    `db:"volume_id" json:"volume_id"`
	ScheduleID *string    `db:"schedule_id" json:"schedule_id"`
	DestID     string     `db:"dest_id" json:"dest_id"`
	Trigger    string     `db:"trigger" json:"trigger"`
	Status     string     `db:"status" json:"status"`
	ObjectKey  string     `db:"object_key" json:"object_key"`
	SizeBytes  int64      `db:"size_bytes" json:"size_bytes"`
	Error      string     `db:"error" json:"error"`
	CreatedAt  time.Time  `db:"created_at" json:"created_at"`
	FinishedAt *time.Time `db:"finished_at" json:"finished_at"`
}

type BackupRunStore interface {
	Create(ctx context.Context, b BackupRun) error
	Get(ctx context.Context, id string) (BackupRun, error)
	ListByVolume(ctx context.Context, volumeID string) ([]BackupRun, error)
	ListByKind(ctx context.Context, kind string) ([]BackupRun, error)
	// ListByPrefix lists the runs whose object key sits under prefix.
	ListByPrefix(ctx context.Context, prefix string) ([]BackupRun, error)
	Update(ctx context.Context, b BackupRun) error
	Delete(ctx context.Context, id string) error
}

var backupRunsT = newTable[BackupRun]("backup_runs", nil)

type backupRuns struct{ crud[BackupRun] }

func (s backupRuns) ListByVolume(ctx context.Context, volumeID string) ([]BackupRun, error) {
	return s.many(ctx, "volume_id = ?", volumeID)
}

func (s backupRuns) ListByKind(ctx context.Context, kind string) ([]BackupRun, error) {
	return s.many(ctx, "kind = ?", kind)
}

func (s backupRuns) ListByPrefix(ctx context.Context, prefix string) ([]BackupRun, error) {
	return s.many(ctx, "substr(object_key, 1, ?) = ?", len(prefix), prefix)
}
