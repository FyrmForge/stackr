package volume

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
)

func TestEveryTabRenders(t *testing.T) {
	v := View{
		Node:  "v1",
		Name:  "uploads",
		Scope: "env",
		Base:  "/o/s/e/-/volumes/v1",
	}
	b := BackupsView{
		ID:   "v1",
		Name: "uploads",
		Base: v.Base,
		Edit: true,
		Schedules: []Schedule{{
			ID:     "s1",
			Method: "volume",
			Cron:   "0 2 * * *",
			Dest:   "b2",
			Keep:   "7",
		}},
		Runs: []Run{{
			ID:         "r1",
			Status:     "done",
			When:       time.Date(2026, 7, 1, 2, 0, 0, 0, time.UTC),
			Size:       "1.2 MB",
			Restorable: true,
		}},
		Dests: []Option{
			{"", "local disk"},
			{"d1", "b2"},
		},
		Methods: []string{"volume"},
		Live:    true,
	}
	for tab, tc := range map[string]struct {
		body templ.Component
		want []string
	}{
		"overview": {
			Overview(OverviewView{Docker: "stackr-vol-v1", Mounts: []Mount{{Tile: "web", Path: "/data"}}, Orphaned: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)}),
			[]string{"stackr-vol-v1", "/data", "Orphaned since <local-time fmt=\"Jan 2, 15:04\"><time datetime=\"2026-07-02T10:00:00Z\" title=\"2026-07-02T10:00:00Z\">Jul 2, 10:00</time></local-time>: no tile mounts it."},
		},
		"backups": {
			Backups(b),
			[]string{"Every day at 02:00", "Volume archive", "/backup\"", "/restore?run=r1", "local disk", "/schedules\"", "/schedules/s1/delete", `hx-trigger="every 2s"`, `hx-select="#backup-history-v1"`},
		},
		"settings": {
			Settings(v, "", nil),
			[]string{"Delete volume", `word="uploads"`, "/-/volumes/v1/delete"},
		},
	} {
		var sb strings.Builder
		if err := Drawer(v, tc.body).Render(context.Background(), &sb); err != nil {
			t.Fatal(err)
		}
		for _, w := range append(tc.want, `id="drawer-view"`, "Overview", "Backups") {
			if !strings.Contains(sb.String(), w) {
				t.Errorf("%s lacks %q", tab, w)
			}
		}
	}
}

// After an action History polls a few times, counting down, then stops.
func TestHistoryPollCountsDown(t *testing.T) {
	for poll, want := range map[int]string{3: `poll=2"`, 1: `poll=0"`, 0: ""} {
		var sb strings.Builder
		if err := History(BackupsView{ID: "v1", Base: "/v", Poll: poll}).Render(context.Background(), &sb); err != nil {
			t.Fatal(err)
		}
		got := strings.Contains(sb.String(), "every 2s")
		if got != (want != "") || !strings.Contains(sb.String(), want) {
			t.Errorf("poll %d: polling %v\n%s", poll, got, sb.String())
		}
	}
}

// A mounted volume offers no delete.
func TestMountedHasNoDelete(t *testing.T) {
	var sb strings.Builder
	if err := Settings(View{Name: "uploads"}, "Detach before deleting.", nil).Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), "Delete volume") || !strings.Contains(sb.String(), "Detach before deleting") {
		t.Error("a mounted volume must not offer delete")
	}
}

// The Backups tab: a flat group per schedule with its facts, the Add a
// schedule form with nothing prefilled, and the empty state's one sentence.
func TestBackupsFacts(t *testing.T) {
	render := func(b BackupsView) string {
		var sb strings.Builder
		if err := Backups(b).Render(context.Background(), &sb); err != nil {
			t.Fatal(err)
		}
		return sb.String()
	}
	b := BackupsView{ID: "v1", Name: "pgdata", Base: "/v", Edit: true, Methods: []string{"dump"},
		Schedules: []Schedule{{ID: "s1", Method: "dump", Cron: "0 3 * * *", TZ: "UTC", Dest: "local disk", Keep: "7", Mode: "pause"}}}
	got := render(b)
	for _, w := range []string{"Database dump", "to local disk", "Every day at 03:00", "<span>UTC</span>", "Keep", "While it runs", `aria-label="Edit Keep"`, "Add a schedule", "Back up now", `name="repeat"`, "Empty uses 03:00."} {
		if !strings.Contains(got, w) {
			t.Errorf("no %q in\n%s", w, got)
		}
	}
	if strings.Contains(got, "0 3 * * *") { // the readable line is not printed again raw
		t.Errorf("the cron line is printed beside its words\n%s", got)
	}
	if strings.Contains(got, "No backups are scheduled.") || strings.Contains(got, "default</span>") {
		t.Errorf("a schedule row carries the empty state or the default tag\n%s", got)
	}
	b.Schedules = nil
	if got = render(b); !strings.Contains(got, "No backups are scheduled.") {
		t.Errorf("no empty state\n%s", got)
	}
	b.Edit = false
	if got = render(b); strings.Contains(got, "Add a schedule") {
		t.Errorf("a viewer sees Add a schedule\n%s", got)
	}
}
