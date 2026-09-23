package testdb

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// BackupRuns satisfies infra/backup.Runs and CronRuns satisfies
// infra/jobs.Runs, both by writing the store directly.
//
// The real implementations are service.BackupScheduleService and
// service.TileLifecycleService. Both infra packages are ones service/ is
// built on, so their in-package tests cannot name it. Same shape, and same
// caveat, as WorkItems and NodeRows: the methods they double carry no rule
// today, and if one of them grows one these tests should fail rather than
// quietly keep passing.
//
// They exist because the two services are handed over by a setter after
// construction, so a test that builds the runner and never calls WithRuns
// gets a nil interface and panics on the first run — which is exactly what
// TestRestartFailsAnInterruptedBackup and TestStopAWaitingRun did.
type BackupRuns struct{ Store repo.Store }

func (b BackupRuns) OpenRun(ctx context.Context, r *repo.BackupRun) error {
	return b.Store.CreateBackupRun(ctx, r)
}

func (b BackupRuns) SaveRun(ctx context.Context, r *repo.BackupRun) error {
	return b.Store.UpdateBackupRun(ctx, r)
}

type CronRuns struct{ Store repo.Store }

func (c CronRuns) StartRun(ctx context.Context, r *repo.CronRun) error {
	return c.Store.CreateCronRun(ctx, r)
}

func (c CronRuns) FinishRun(ctx context.Context, r *repo.CronRun) error {
	return c.Store.FinishCronRun(ctx, r)
}

func (c CronRuns) PruneRuns(ctx context.Context, before time.Time) error {
	return c.Store.PruneCronRuns(ctx, before)
}

func (c CronRuns) RecordTileRun(ctx context.Context, tileID, status, output string) error {
	return c.Store.RecordTileRun(ctx, tileID, status, output)
}
