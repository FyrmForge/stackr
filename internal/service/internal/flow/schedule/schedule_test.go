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
		Crons: func(context.Context) ([]store.Tile, error) {
			return []store.Tile{{ID: "t1", Schedule: "*/5 * * * *"}, {ID: "t2", Schedule: "* * * * *", Paused: true}}, nil
		},
		Cron: func(_ context.Context, t store.Tile) error { got = append(got, "cron "+t.ID); return nil },
	}
	scheds, _ := d.Schedules(ctx)
	crons, _ := d.Crons(ctx)
	es := Entries(d, scheds, crons)
	want := map[string]string{"orphans": "@daily", "image-watch": "@every 1m",
		"backup b1": "CRON_TZ=Europe/London 0 3 * * *", "backup b2": "not a cron", "cron t1": "*/5 * * * *"}
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
