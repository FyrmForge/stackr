package deployment

import (
	"strings"
	"testing"
	"time"
)

// The viewer renders one row per SSE event and parses "O <ts> <msg>", so a
// multi-line chunk has to leave as several events, each framed. Blank lines
// would render as a timestamp with no message, so they are dropped.
func TestFrameLog(t *testing.T) {
	var b strings.Builder
	ts := time.Date(2026, 9, 5, 10, 30, 0, 0, time.UTC)
	frameLog(&b, "step 1\r\n\nstep 2\n", ts)
	want := "data: O 2026-09-05T10:30:00Z step 1\n\n" +
		"data: O 2026-09-05T10:30:00Z step 2\n\n"
	if b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
}
