package jobs_test

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
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

// A job that deploys tile by tile has a step per tile: only the tile being
// deployed has one, and a job that never says "deploying" has one for all.
func TestPhasePerTile(t *testing.T) {
	g := newGate(false)
	h := func(ctx context.Context, r *jobs.Run) error {
		for _, l := range []string{"deploying api\n", "pulling api:1\n", "deploying web\n", "creating web\n"} {
			_, _ = r.Log.Write([]byte(l))
		}
		return g.run(ctx, r)
	}
	r, l := setup(t, map[jobs.Kind]jobs.Handler{"a": h}, jobs.Options{})
	start(t, r)
	id := enqueue(t, r, "a", "t1", "t2")
	started(t, g)
	if got := r.Phase(id, "web"); got != "Starting" {
		t.Errorf("web = %q", got)
	}
	if got := r.Phase(id, "api"); got != "" {
		t.Errorf("api is done, phase = %q", got)
	}
	close(g.release)
	waitState(t, l, id, job.Done)

	s := newGate(false)
	one := func(ctx context.Context, r *jobs.Run) error {
		_, _ = r.Log.Write([]byte("pulling x\n"))
		return s.run(ctx, r)
	}
	r2, _ := setup(t, map[jobs.Kind]jobs.Handler{"a": one}, jobs.Options{})
	start(t, r2)
	id2 := enqueue(t, r2, "a", "t1")
	started(t, s)
	if got := r2.Phase(id2, "anything"); got != "Pulling image" {
		t.Errorf("single-tile job phase = %q", got)
	}
}
