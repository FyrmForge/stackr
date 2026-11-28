package service

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func TestSetVolumeBackup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newWorld(t)
	o := w.orch
	v, _, err := o.DeclareEnvVolume(ctx, w.env, "data")
	must(t, err)

	got := func(wantCron string, wantKeep, wantN int) {
		t.Helper()
		c, k, n, err := o.volumeBackup(ctx, v.ID)
		must(t, err)
		if c != wantCron || k != wantKeep || n != wantN {
			t.Fatalf("volumeBackup = %q %d %d, want %q %d %d", c, k, n, wantCron, wantKeep, wantN)
		}
	}
	got("", 0, 0)
	if !o.noBackup(ctx, []store.Volume{v})[v.ID] {
		t.Error("noBackup false for a bare volume")
	}

	// no off-box destination: local
	must(t, o.setVolumeBackup(ctx, v.ID, " 0  3 * * * ", 5))
	got("0 3 * * *", 5, 1)
	ss, _ := o.backups.Schedules(ctx, v.ID)
	local, err := o.localDest(ctx)
	must(t, err)
	if ss[0].DestID != nil && *ss[0].DestID != local.ID {
		t.Errorf("dest = %v, want local", *ss[0].DestID)
	}
	if o.noBackup(ctx, []store.Volume{v})[v.ID] {
		t.Error("noBackup true with a schedule")
	}
	if ok, err := o.VolumeHasBackup(ctx, v.ID); err != nil || !ok {
		t.Errorf("VolumeHasBackup = %v, %v", ok, err)
	}

	// change in place, keep the id
	id := ss[0].ID
	must(t, o.setVolumeBackup(ctx, v.ID, "@daily", 7))
	got("@daily", 7, 1)
	if ss, _ := o.backups.Schedules(ctx, v.ID); ss[0].ID != id {
		t.Error("a change replaced the schedule")
	}

	// extra schedules fold into one; a base schedule is left alone
	must(t, w.st.BackupSchedules.Create(ctx, store.BackupSchedule{ID: "s-x", VolumeID: v.ID, Method: "tar",
		Cron: "@hourly", Timezone: "UTC", Keep: 1, Mode: "live", CreatedAt: time.Now()}))
	must(t, w.st.BackupSchedules.Create(ctx, store.BackupSchedule{ID: "s-base", VolumeID: v.ID, Method: "base",
		Cron: "@weekly", Timezone: "UTC", Keep: 1, Mode: "live", CreatedAt: time.Now()}))
	got("@daily", 7, 2)
	must(t, o.setVolumeBackup(ctx, v.ID, "@daily", 7))
	got("@daily", 7, 1)

	// none
	must(t, o.setVolumeBackup(ctx, v.ID, "", 0))
	got("", 0, 0)
	if ss, _ := o.backups.Schedules(ctx, v.ID); len(ss) != 1 || ss[0].Method != "base" {
		t.Errorf("after none: %+v, want the base schedule only", ss)
	}
}

func TestSetVolumeBackupPicksOffBox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newWorld(t)
	o := w.orch
	_, srv := newFakeS3(t)
	d, err := o.CreateBackupDest(ctx, nil, BackupDestSpec{
		Name: "offsite", Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s", Shared: true,
	})
	must(t, err)
	v, _, err := o.DeclareEnvVolume(ctx, w.env, "data")
	must(t, err)
	must(t, o.setVolumeBackup(ctx, v.ID, "@daily", 7))
	ss, _ := o.backups.Schedules(ctx, v.ID)
	if len(ss) != 1 || ss[0].DestID == nil || *ss[0].DestID != d.ID {
		t.Fatalf("schedules = %+v, want one to %s", ss, d.ID)
	}
}
