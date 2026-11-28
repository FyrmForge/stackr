package graph

import "testing"

// A paused tile ranks with stopped: below degraded, above running.
func TestPausedRank(t *testing.T) {
	if rank["paused"] != rank["stopped"] {
		t.Fatalf("paused %d, stopped %d", rank["paused"], rank["stopped"])
	}
	if worse("running", "paused") != "paused" || worse("degraded", "paused") != "degraded" {
		t.Fatal("paused must outrank running and yield to degraded")
	}
}
