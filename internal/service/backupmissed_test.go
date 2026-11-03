package service

import (
	"context"
	"strings"
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

// A volume that never had a schedule or a backup gets @daily keep 7 on the
// watch; one that had runs but lost its schedule gets the warning instead.
func TestBackupWatchDefaultsAndWarns(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	o := w.orch
	fresh := store.Volume{ID: "v-fresh", ScopeKind: "env", ScopeID: w.env, Slug: "fresh", Name: "fresh", CreatedAt: time.Now()}
	bare := store.Volume{ID: "v-bare", ScopeKind: "env", ScopeID: w.env, Slug: "bare", Name: "bare", CreatedAt: time.Now()}
	must(t, w.st.Volumes.Create(ctx, fresh))
	must(t, w.st.Volumes.Create(ctx, bare))
	local, err := o.localDest(ctx)
	must(t, err)
	r, err := o.backups.Start(ctx, backup.KindVolume, &bare.ID, nil, local.ID, "manual")
	must(t, err)
	_, err = o.backups.Finish(ctx, r, "k", 1, nil)
	must(t, err)

	o.backupWatch(ctx)
	ss, err := o.backups.Schedules(ctx, fresh.ID)
	must(t, err)
	if len(ss) != 1 || ss[0].Cron != "@daily" || ss[0].Keep != 7 {
		t.Errorf("fresh volume schedules = %+v", ss)
	}
	if hasRow(t, o, "problem.backup.none."+fresh.ID) {
		t.Error("fresh volume warned")
	}
	if !hasRow(t, o, "problem.backup.none."+bare.ID) {
		t.Error("unscheduled volume with history has no warning")
	}
	if ps, err := o.Problems(ctx); err != nil || len(ps) == 0 {
		t.Fatalf("problems = %+v, %v; want the no-backup warning", ps, err)
	}
	_, err = o.volumes.Orphan(ctx, bare)
	must(t, err)
	ps, err := o.Problems(ctx)
	must(t, err)
	for _, p := range ps {
		if strings.Contains(p.Key, bare.ID) {
			t.Errorf("orphaned volume still listed: %+v", p)
		}
	}
	if hasRow(t, o, "problem.backup.none."+bare.ID) {
		t.Error("orphaned volume's problem row not cleared")
	}
}
