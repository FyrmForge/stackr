package tile

import "testing"

// A container line carries docker's stream mark and timestamp; both come
// off, the time kept apart. A line without them stays whole.
func TestLogLine(t *testing.T) {
	for in, want := range map[string][2]string{
		"O 2026-09-25T00:11:08.057377964Z done":  {"00:11:08", "done"},
		"E 2026-09-25T00:11:08.057377964Z ERROR": {"00:11:08", "ERROR"},
		"2026-09-25T00:11:08Z plain":             {"00:11:08", "plain"},
		"O not a time":                           {"", "O not a time"},
	} {
		l := logLine(in)
		if l.Time != want[0] || l.Text != want[1] {
			t.Errorf("logLine(%q) = %q %q, want %q", in, l.Time, l.Text, want)
		}
	}
}
