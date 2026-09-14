package app

import (
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Replayed lines carry the run's short id as the "tag|" prefix the viewer
// turns into a chip, then the mark and the run's start time.
func TestReplayRun(t *testing.T) {
	started := time.Date(2026, 9, 5, 10, 30, 0, 0, time.UTC)
	r := repo.CronRun{ID: "0123456789abcdef", StartedAt: started, Output: "hello\nworld\n"}
	var b strings.Builder
	replayRun(&b, r)
	want := "data: 01234567|O 2026-09-05T10:30:00Z hello\n\n" +
		"data: 01234567|O 2026-09-05T10:30:00Z world\n\n"
	if b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}

	// A run that stored nothing writes nothing: a bare "O <ts> " is an empty row.
	b.Reset()
	replayRun(&b, repo.CronRun{ID: "0123456789abcdef", StartedAt: started})
	if b.String() != "" {
		t.Errorf("empty output produced %q", b.String())
	}
}
