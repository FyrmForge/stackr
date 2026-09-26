package volume

import (
	"context"
	"strings"
	"testing"

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
		Schedules: []Schedule{{
			Method: "volume",
			Cron:   "0 2 * * *",
			Dest:   "b2",
			Keep:   "7",
		}},
		Runs: []Run{{
			ID:         "r1",
			Status:     "done",
			When:       "Jul 1 02:00",
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
			Overview(OverviewView{Docker: "stackr-vol-v1", Mounts: []Mount{{Tile: "web", Path: "/data"}}, Orphaned: "Jul 2 10:00"}),
			[]string{"stackr-vol-v1", "/data", "Orphaned since Jul 2 10:00"},
		},
		"backups": {
			Backups(b),
			[]string{"0 2 * * *", "Volume archive", "/backup\"", "/restore?run=r1", "local disk", `hx-trigger="every 2s"`, `hx-select="#backup-history-v1"`},
		},
		"settings": {
			Settings(v, ""),
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
	if err := Settings(View{Name: "uploads"}, "Detach before deleting.").Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), "Delete volume") || !strings.Contains(sb.String(), "Detach before deleting") {
		t.Error("a mounted volume must not offer delete")
	}
}
