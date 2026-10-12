package admin

import (
	"testing"
	"time"
)

func TestUptime(t *testing.T) {
	for d, want := range map[time.Duration]string{
		285 * time.Hour:              "11 days",
		3*time.Hour + 20*time.Minute: "3 h 20 min",
		5 * time.Minute:              "5 min",
	} {
		if got := uptime(d); got != want {
			t.Errorf("uptime(%v) = %q, want %q", d, got, want)
		}
	}
}
