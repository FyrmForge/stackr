package service

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A schedule with no run for longer than its interval runs once on the watch;
// a volume with no good backup in two intervals alerts once, and a good run
// clears it.
func TestBackupWatchCatchesUpAndAlerts(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	o := w.orch
	vol := store.Volume{ID: "v-old", ScopeKind: "env", ScopeID: w.env, Slug: "data", Name: "data", CreatedAt: time.Now()}
	must(t, w.st.Volumes.Create(ctx, vol))
	must(t, w.st.BackupSchedules.Create(ctx, store.BackupSchedule{ID: "s-old", VolumeID: vol.ID, Method: "tar",
		Cron: "0 3 * * *", Timezone: "UTC", Keep: 3, Mode: "live", CreatedAt: time.Now().AddDate(0, 0, -4)}))

	o.backupWatch(ctx)
	if !hasRow(t, o, "problem.backup.stale."+vol.ID) || !hasRow(t, o, "alert.backup.stale."+vol.ID) {
		t.Fatal("no stale problem for a volume with no backup in 4 days")
	}
	js, err := w.st.Jobs.ListByState(ctx, "queued", "running", "done", "failed")
	must(t, err)
	var backups int
	for _, j := range js {
		if j.Kind == string(kindBackup) {
			backups++
		}
	}
	if backups != 1 {
		t.Errorf("%d catch-up backup jobs, want 1", backups)
	}

	local, err := o.localDest(ctx)
	must(t, err)
	r, err := o.backups.Start(ctx, backup.KindVolume, &vol.ID, nil, local.ID, "schedule")
	must(t, err)
	_, err = o.backups.Finish(ctx, r, "stackr/o/v-old/s/x.age", 1, nil)
	must(t, err)
	o.backupWatch(ctx)
	if hasRow(t, o, "problem.backup.stale."+vol.ID) || hasRow(t, o, "alert.backup.stale."+vol.ID) {
		t.Error("a good backup left the stale problem open")
	}
}
