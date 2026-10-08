package service

import (
	"context"
	"slices"
	"testing"
)

// A schedule knob change reloads the cron table at once; a bad line is
// refused and leaves the table alone.
func TestSetSettingReloadsSchedule(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	has := func(name string) bool { return slices.Contains(w.orch.sched.Names(), name) }
	if !has("panel-backup") || !has("orphans") || has("cleanup") {
		t.Fatalf("defaults: %v", w.orch.sched.Names())
	}
	if err := w.orch.SetSetting(ctx, "panel_backup_schedule", "off"); err == nil {
		t.Error("`off` was taken as a cron line")
	}
	if err := w.orch.SetSetting(ctx, "panel_backup_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	if has("panel-backup") {
		t.Errorf("switched off still scheduled: %v", w.orch.sched.Names())
	}
	if err := w.orch.SetSetting(ctx, "orphans_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	if has("orphans") {
		t.Errorf("switched off still scheduled: %v", w.orch.sched.Names())
	}
	if err := w.orch.SetSetting(ctx, "orphans_enabled", ""); err != nil {
		t.Fatal(err)
	}
	if err := w.orch.SetSetting(ctx, "cleanup_enabled", "true"); err != nil {
		t.Fatal(err)
	}
	if !has("cleanup") {
		t.Errorf("cleanup_enabled did not reload: %v", w.orch.sched.Names())
	}
	if err := w.orch.SetSetting(ctx, "orphans_schedule", "not a cron"); err == nil {
		t.Error("a bad cron line was taken")
	}
	if !has("orphans") {
		t.Errorf("a refused write dropped the entry: %v", w.orch.sched.Names())
	}
	if err := w.orch.SetSetting(ctx, "orphans_schedule", "CRON_TZ=Europe/London 0 1 * * *"); err != nil {
		t.Errorf("a CRON_TZ line was refused: %v", err)
	}
}
