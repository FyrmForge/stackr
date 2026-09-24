package volume

import (
	"context"
	"strings"
	"testing"
)

func TestBackupsRenders(t *testing.T) {
	var b strings.Builder
	err := Backups(View{
		Name:     "uploads",
		Scope:    "env",
		Orphaned: "Jul 2 10:00",
		Base:     "/o/s/e/-/volumes/v1",
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
	}).Render(context.Background(), &b)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"0 2 * * *",
		"/backup\"",
		"/restore?run=r1",
		"Delete volume",
		`word="uploads"`,
		"local disk",
	} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("lacks %q", w)
		}
	}
}
