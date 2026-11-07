package env

import "testing"

// A datetime-local value is read as UTC; RFC3339 and latest pass through.
func TestAtUTC(t *testing.T) {
	for in, want := range map[string]string{
		"2026-11-07T10:42":          "2026-11-07T10:42:00Z",
		"2026-11-07T10:42:13":       "2026-11-07T10:42:13Z",
		"\t2026-11-07T10:42\n":      "2026-11-07T10:42:00Z", // padding is trimmed
		"2026-11-07T10:42:00+02:00": "2026-11-07T10:42:00+02:00",
		"latest":                    "latest",
		"":                          "",
	} {
		if got := atUTC(in); got != want {
			t.Errorf("atUTC(%q) = %q, want %q", in, got, want)
		}
	}
}
