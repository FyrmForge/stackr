package schedule

import (
	"context"
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
		Backup:  func(_ context.Context, s store.BackupSchedule) error { got = append(got, "backup "+s.ID); return nil },
		Orphans: func(context.Context) error { got = append(got, "orphans"); return nil },
		Watch:   func(context.Context) error { got = append(got, "watch"); return nil },
	}
	scheds, _ := d.Schedules(ctx)
	es := Entries(d, scheds)
	want := map[string]string{"orphans": "@daily", "image-watch": "@every 1m",
		"backup b1": "CRON_TZ=Europe/London 0 3 * * *", "backup b2": "not a cron"}
	for _, e := range es {
		if want[e.Name] != e.Spec {
			t.Errorf("%s = %q, want %q", e.Name, e.Spec, want[e.Name])
		}
		_ = e.Fire(ctx)
	}
	if len(es) != 4 || got[0] != "orphans" || got[1] != "watch" || got[2] != "backup b1" || got[3] != "backup b2" {
		t.Errorf("fired %v", got)
	}

	// The runner registers the good entries and names the bad one.
	r := New(d)
	defer r.Stop()
	if err := r.Boot(ctx); err == nil || len(r.entries) != 3 {
		t.Errorf("boot: %v, %d entries", err, len(r.entries))
	}
	r.Reload(ctx) // rebuild, never doubled
	if len(r.cron.Entries()) != 3 {
		t.Errorf("after reload: %d cron entries", len(r.cron.Entries()))
	}
}
