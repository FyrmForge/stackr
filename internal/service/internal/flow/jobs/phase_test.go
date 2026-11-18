package jobs_test

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
)

// Only the deploy's own step lines name a phase; progress noise names none.
func TestPhaseOf(t *testing.T) {
	for line, want := range map[string]string{
		"pulling nginx:1\n":                      "Pulling image",
		"Building main at abc1234.\n":            "Building image",
		"starting web\n":                         "Starting",
		"creating web\n":                         "Starting",
		"waiting for pg to accept connections\n": "Health check",
		"waiting for pg to be healthy\n":         "Waiting for pg to be healthy",
		"#5 [2/4] RUN make\n":                    "",
		"pulling a\npulling b\n":                 "",
		"deploying web\n":                        "",
		"waiting for the run of web to end\n":    "",
	} {
		if got := jobs.PhaseOf(line); got != want {
			t.Errorf("PhaseOf(%q) = %q, want %q", line, got, want)
		}
	}
}
