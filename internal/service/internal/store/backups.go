package store

import (
	"context"
	"time"
)

// BackupDest is a row of backup_destinations. OrgID nil = admin-global.
type BackupDest struct {
	ID         string    `db:"id"`
	OrgID      *string   `db:"org_id"`
	Kind       string    `db:"kind"`
	Name       string    `db:"name"`
	Endpoint   string    `db:"endpoint"`
	Region     string    `db:"region"`
	Bucket     string    `db:"bucket"`
	AccessKey  string    `db:"access_key"`
	SecretKey  string    `db:"secret_key"`
	ArchiveKey string    `db:"archive_key"`
	Shared     bool      `db:"shared"`
	CreatedAt  time.Time `db:"created_at"`
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
	ID        string    `db:"id"`
	VolumeID  string    `db:"volume_id"`
	Method    string    `db:"method"`
	DestID    *string   `db:"dest_id"`
	Cron      string    `db:"cron"`
	Timezone  string    `db:"timezone"`
	Keep      int       `db:"keep"`
	Mode      string    `db:"mode"`
	CreatedAt time.Time `db:"created_at"`
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
	ID         string     `db:"id"`
	Kind       string     `db:"kind"`
	VolumeID   *string    `db:"volume_id"`
	ScheduleID *string    `db:"schedule_id"`
	DestID     string     `db:"dest_id"`
	Trigger    string     `db:"trigger"`
	Status     string     `db:"status"`
	ObjectKey  string     `db:"object_key"`
	SizeBytes  int64      `db:"size_bytes"`
	Error      string     `db:"error"`
	CreatedAt  time.Time  `db:"created_at"`
	FinishedAt *time.Time `db:"finished_at"`
}

type BackupRunStore interface {
	Create(ctx context.Context, b BackupRun) error
	Get(ctx context.Context, id string) (BackupRun, error)
	ListByVolume(ctx context.Context, volumeID string) ([]BackupRun, error)
	ListByKind(ctx context.Context, kind string) ([]BackupRun, error)
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
