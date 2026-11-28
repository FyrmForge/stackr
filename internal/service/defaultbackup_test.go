package service

import (
	"context"
	"testing"
)

// A new volume gets no schedule, with or without an off-box destination.
func TestNewVolumeGetsNoSchedule(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	v, _, err := w.orch.DeclareEnvVolume(ctx, w.env, "plain")
	must(t, err)
	if ss, _ := w.orch.BackupSchedules(ctx, v.ID); len(ss) != 0 {
		t.Fatalf("no destination: %d schedules", len(ss))
	}
	_, srv := newFakeS3(t)
	_, err = w.orch.CreateBackupDest(ctx, nil, BackupDestSpec{
		Name: "offsite", Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s", Shared: true,
	})
	must(t, err)
	v, _, err = w.orch.DeclareEnvVolume(ctx, w.env, "data")
	must(t, err)
	if ss, _ := w.orch.BackupSchedules(ctx, v.ID); len(ss) != 0 {
		t.Fatalf("with destination: %d schedules", len(ss))
	}
}
