package instance

import (
	"context"
	"strings"
	"testing"
	"time"
)

func renderPITR(t *testing.T, p PITRView) string {
	t.Helper()
	var b strings.Builder
	if err := pitr(View{Base: "/o/s/e/-/instances/pg"}, p).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Off: the plain statement and a switch that says what it costs; read-only
// viewers get the statement alone.
func TestPITROff(t *testing.T) {
	got := renderPITR(t, PITRView{Edit: true})
	for _, want := range []string{"Point-in-time restore is off.", `hx-post="/o/s/e/-/instances/pg/pitr"`, `name="days"`, `value="7"`, `min="1"`, `max="365"`, "Days to keep", "Restarts the database once"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if got := renderPITR(t, PITRView{}); strings.Contains(got, "<form") {
		t.Error("a read-only viewer got the switch")
	}
}

// On: the range, the last base backup, the archive, and a local-disk label.
func TestPITROn(t *testing.T) {
	now := time.Now()
	got := renderPITR(t, PITRView{
		On: true, Edit: true, Days: 7, Dest: "offsite", Local: true, From: now.Add(-48 * time.Hour), To: now,
		Base: &PITRBase{At: now, Error: "pg_basebackup: no space", OK: false}, Bundles: 12, Size: "3.2 MB", Waiting: 2,
	})
	for _, want := range []string{
		"Restorable from", "Last base backup: failed,", "pg_basebackup: no space",
		"12 bundles, 3.2 MB on offsite; 2 segments waiting", "not a lost server", "/instances/pg/pitr?days=0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if got := renderPITR(t, PITRView{On: true}); !strings.Contains(got, "Restorable: not yet") || !strings.Contains(got, "none yet") {
		t.Errorf("no base yet: %s", got)
	}
}
