package schedule

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func TestScheduleToCall(t *testing.T) {
	ctx := context.Background()
	var got []string
	d := Drivers{
		Schedules: func(context.Context) ([]store.BackupSchedule, error) {
			return []store.BackupSchedule{
				{ID: "b1", Cron: "0 3 * * *", Timezone: "Europe/London"},
				{ID: "b2", Cron: "not a cron"},
			}, nil
		},
		Backup: func(_ context.Context, s store.BackupSchedule) error {
			got = append(got, "backup "+s.ID)
			return nil
		},
		Orphans: func(context.Context) error {
			got = append(got, "orphans")
			return nil
		},
		Watch: func(context.Context) error {
			got = append(got, "watch")
			return nil
		},
		Crons: func(context.Context) ([]store.Tile, error) {
			return []store.Tile{
				{ID: "t1", Schedule: "*/5 * * * *"},
				{ID: "t2", Schedule: "* * * * *", Paused: true},
			}, nil
		},
		Cron: func(_ context.Context, t store.Tile) error {
			got = append(got, "cron "+t.ID)
			return nil
		},
	}
	scheds, _ := d.Schedules(ctx)
	crons, _ := d.Crons(ctx)
	es := Entries(d, scheds, crons, nil)
	want := map[string]string{
		"orphans":     "@daily",
		"image-watch": "@every 1m",
		"backup b1":   "CRON_TZ=Europe/London 0 3 * * *",
		"backup b2":   "not a cron",
		"cron t1":     "*/5 * * * *",
	}
	for _, e := range es {
		if want[e.Name] != e.Spec {
			t.Errorf("%s = %q, want %q", e.Name, e.Spec, want[e.Name])
		}
		_ = e.Fire(ctx)
	}
	if len(es) != 5 || got[0] != "orphans" || got[1] != "watch" || got[2] != "backup b1" || got[3] != "backup b2" ||
		got[4] != "cron t1" {
		t.Errorf("fired %v", got)
	}

	// The runner registers the good entries and names the bad one.
	r := New(d)
	defer r.Stop()
	if err := r.Boot(ctx); err == nil || len(r.entries) != 4 {
		t.Errorf("boot: %v, %d entries", err, len(r.entries))
	}
	r.Reload(ctx) // rebuild, never doubled
	if len(r.cron.Entries()) != 4 {
		t.Errorf("after reload: %d cron entries", len(r.cron.Entries()))
	}
}

// The traffic sample gets its own 5 s entry when wired.
func TestTrafficEntry(t *testing.T) {
	es := Entries(Drivers{Traffic: func(context.Context) error { return nil }}, nil, nil, nil)
	if last := es[len(es)-1]; last.Name != "traffic" || last.Spec != "@every 5s" {
		t.Errorf("entries = %+v", es)
	}
}

func specs(es []Entry) map[string]string {
	m := map[string]string{}
	for _, e := range es {
		m[e.Name] = e.Spec
	}
	return m
}

func settingsDrivers() Drivers {
	f := func(context.Context) error { return nil }
	return Drivers{Orphans: f, Watch: f, PanelBackup: f, Cleanup: f}
}

// The knobs drive the entries: defaults, an override, cleanup off, off, a bad line.
func TestSettingEntries(t *testing.T) {
	d := settingsDrivers()
	// Defaults: cleanup_enabled is false, so no cleanup entry.
	got := specs(Entries(d, nil, nil, nil))
	if got["orphans"] != "@daily" || got["panel-backup"] != "0 3 * * *" || got["image-watch"] == "" {
		t.Errorf("defaults = %v", got)
	}
	if _, ok := got["cleanup"]; ok {
		t.Errorf("cleanup scheduled while disabled: %v", got)
	}
	// Overrides, cleanup on at its default time.
	got = specs(Entries(d, nil, nil, map[string]string{
		"orphans_schedule":      "0 1 * * *",
		"panel_backup_schedule": "CRON_TZ=Europe/London 0 2 * * *",
		"cleanup_enabled":       "true",
		"cleanup_schedule":      "30 4 * * *",
	}))
	if got["orphans"] != "0 1 * * *" || got["panel-backup"] != "CRON_TZ=Europe/London 0 2 * * *" || got["cleanup"] != "30 4 * * *" {
		t.Errorf("overrides = %v", got)
	}
	// A switch off and a bad line each drop only their own entry.
	got = specs(Entries(d, nil, nil, map[string]string{
		"orphans_enabled":       "false",
		"panel_backup_schedule": "not a cron",
		"cleanup_enabled":       "true",
	}))
	if _, ok := got["orphans"]; ok {
		t.Errorf("switched-off orphans scheduled: %v", got)
	}
	if _, ok := got["panel-backup"]; ok {
		t.Errorf("bad spec scheduled: %v", got)
	}
	if got["cleanup"] != "30 4 * * *" {
		t.Errorf("cleanup = %q", got["cleanup"])
	}
	if got["image-watch"] != "@every 1m" {
		t.Errorf("a bad spec blocked the rest: %v", got)
	}
	got = specs(Entries(d, nil, nil, map[string]string{"panel_backup_enabled": "false"}))
	if _, ok := got["panel-backup"]; ok {
		t.Errorf("switched-off panel backup scheduled: %v", got)
	}
	// An empty schedule is the default time, never off, and `off` is no magic word.
	got = specs(Entries(d, nil, nil, map[string]string{
		"orphans_schedule":      "",
		"panel_backup_schedule": "off",
	}))
	if got["orphans"] != "@daily" {
		t.Errorf("empty orphans spec = %q, want the default", got["orphans"])
	}
	if _, ok := got["panel-backup"]; ok {
		t.Errorf("`off` still switches a schedule off: %v", got)
	}
}

// A runner with Settings loads the knob entries and a reload picks up a change.
func TestSettingsReload(t *testing.T) {
	ctx := context.Background()
	d := settingsDrivers()
	set := map[string]string{"panel_backup_schedule": "0 3 * * *"}
	d.Settings = func(context.Context) (map[string]string, error) { return set, nil }
	d.Schedules = func(context.Context) ([]store.BackupSchedule, error) { return nil, nil }
	r := New(d)
	defer r.Stop()
	if err := r.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	has := func() bool { return slices.Contains(r.Names(), "panel-backup") }
	if !has() {
		t.Fatalf("names = %v", r.Names())
	}
	set = map[string]string{"panel_backup_enabled": "false"}
	r.Reload(ctx)
	if has() {
		t.Errorf("switched off still scheduled: %v", r.Names())
	}
}

// A settings read that fails keeps the table as it was: the defaults would
// switch a backup back on, or an off switch back to on.
func TestReloadKeepsTableOnReadError(t *testing.T) {
	ctx := context.Background()
	d := settingsDrivers()
	set, fail := map[string]string{"panel_backup_enabled": "false"}, false
	d.Settings = func(context.Context) (map[string]string, error) {
		if fail {
			return nil, errors.New("db busy")
		}
		return set, nil
	}
	d.Schedules = func(context.Context) ([]store.BackupSchedule, error) { return nil, nil }
	r := New(d)
	defer r.Stop()
	if err := r.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(r.Names(), "panel-backup") {
		t.Fatalf("setup: %v", r.Names())
	}
	fail = true
	r.Reload(ctx)
	if slices.Contains(r.Names(), "panel-backup") {
		t.Errorf("a failed settings read applied the defaults: %v", r.Names())
	}
}
