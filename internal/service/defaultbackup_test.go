package service

import (
	"context"
	"testing"
)

// A new volume gets a daily backup to an off-box destination its org can use;
// with none (only the local disk) it gets no schedule.
func TestNewVolumeGetsDefaultSchedule(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	v, _, err := w.orch.DeclareEnvVolume(ctx, w.env, "plain")
	must(t, err)
	if ss, _ := w.orch.BackupSchedules(ctx, v.ID); len(ss) != 0 {
		t.Fatalf("no destination: %d schedules", len(ss))
	}
	_, srv := newFakeS3(t)
	d, err := w.orch.CreateBackupDest(ctx, nil, BackupDestSpec{
		Name: "offsite", Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s", Shared: true,
	})
	must(t, err)
	v, _, err = w.orch.DeclareEnvVolume(ctx, w.env, "data")
	must(t, err)
	ss, _ := w.orch.BackupSchedules(ctx, v.ID)
	if len(ss) != 1 || ss[0].DestID == nil || *ss[0].DestID != d.ID || ss[0].Cron != "@daily" {
		t.Fatalf("schedules = %+v", ss)
	}
}
