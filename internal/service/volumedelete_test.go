package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
)

// Deleting a volume orphans it: the row, its data and its runs stay for the
// orphan job, and its schedules go.
func TestDeleteVolumeOrphansAndKeepsRuns(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	v, _, err := w.orch.DeclareEnvVolume(ctx, w.env, "data")
	must(t, err)
	_, err = w.orch.AddBackupSchedule(ctx, v.ID, ScheduleSpec{Cron: "@daily", Keep: 3})
	must(t, err)
	local, err := w.orch.localDest(ctx)
	must(t, err)
	r, err := w.orch.backups.Start(ctx, backup.KindVolume, &v.ID, nil, local.ID, "manual")
	must(t, err)
	_, err = w.orch.backups.Finish(ctx, r, "stackr/o/v/manual/x.age", 1, nil)
	must(t, err)

	must(t, w.orch.DeleteVolume(ctx, v.ID))
	got, err := w.orch.Volume(ctx, v.ID)
	if err != nil || got.OrphanedAt == nil {
		t.Fatalf("volume after delete = %+v, %v; want an orphan", got, err)
	}
	if ss, _ := w.orch.BackupSchedules(ctx, v.ID); len(ss) != 0 {
		t.Errorf("%d schedules left", len(ss))
	}
	if rs, _ := w.orch.BackupRuns(ctx, v.ID); len(rs) != 1 {
		t.Errorf("%d runs left, want 1", len(rs))
	}
}
