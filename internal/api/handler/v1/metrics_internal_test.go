package v1

import (
	"testing"
	"time"
)

// The panel's ranges read 7d and 30d; the API takes them as written.
func TestSpan(t *testing.T) {
	for in, want := range map[string]time.Duration{"15m": 15 * time.Minute, "24h": 24 * time.Hour, "7d": 168 * time.Hour, "30d": 720 * time.Hour} {
		if got, err := span(in); err != nil || got != want {
			t.Errorf("span(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"d", "-1d", "1.5d", "week"} {
		if _, err := span(bad); err == nil {
			t.Errorf("span(%q) took it", bad)
		}
	}
}
