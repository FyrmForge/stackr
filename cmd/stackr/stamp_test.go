package main

import (
	"strings"
	"testing"
	"time"
)

// A *_at column and a log line's time read on this machine's clock, in UTC
// under --utc; --json keeps the API's own string.
func TestTimesFollowTheLocalZone(t *testing.T) {
	was := time.Local
	time.Local = time.FixedZone("CEST", 2*3600)
	t.Cleanup(func() { time.Local = was })
	answers := map[string]string{
		"/tiles/nightly/runs": `[{"id":"r1","status":"ok","created_at":"2026-10-21T10:00:05.5Z","finished_at":null}]`,
		"/logs":               `[{"time":"2026-10-21T10:00:05Z","tile":"api","source":"app","stream":"stdout","msg":"hi"}]`,
	}
	at := []string{"--stack", "shop", "--env", "dev"}
	for _, c := range []struct {
		args []string
		want string
	}{
		{append([]string{"tile", "runs", "nightly"}, at...), "2026-10-21 12:00:05 CEST\t(none)"},
		{append([]string{"--utc", "tile", "runs", "nightly"}, at...), "2026-10-21 10:00:05 UTC\t(none)"},
		{append([]string{"tile", "runs", "nightly", "--json"}, at...), `"created_at": "2026-10-21T10:00:05.5Z"`},
		{append([]string{"logs", "search"}, at...), "Oct 21 12:00:05\tapi"},
		{append([]string{"logs", "search", "--utc"}, at...), "Oct 21 10:00:05\tapi"},
	} {
		fake(t, answers)
		code, out, errw := cli(t, c.args...)
		if code != 0 || !strings.Contains(out, c.want) {
			t.Errorf("%v = %d %q\n%s, want %q", c.args, code, errw, out, c.want)
		}
	}
}
