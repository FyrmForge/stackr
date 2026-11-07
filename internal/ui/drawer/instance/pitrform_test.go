package instance

import (
	"strings"
	"testing"
	"time"
)

// On: the window can be changed, and the restore form takes a UTC time
// inside the restorable range or Latest.
func TestPITRWindowAndTime(t *testing.T) {
	from := time.Date(2026, 11, 1, 10, 0, 0, 500, time.UTC)
	to := time.Date(2026, 11, 7, 9, 30, 15, 900, time.UTC)
	got := renderPITR(t, PITRView{On: true, Edit: true, Days: 5, From: from, To: to})
	for _, want := range []string{
		"Keeps 5 days.", "restarts the database once", `name="days"`, `value="5"`, ">Save<",
		"Time (UTC)", `type="datetime-local"`, `step="1"`, `min="2026-11-01T10:00:01"`, `max="2026-11-07T09:30:15"`,
		`name="at" value="latest"`, `hx-post="/o/s/e/-/instances/pg/restore"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	got = renderPITR(t, PITRView{On: true, Edit: true, Days: 5, From: from})
	if strings.Contains(got, `max="0001`) {
		t.Errorf("a zero To became a max: %s", got)
	}
}
