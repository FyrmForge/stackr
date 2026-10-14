package jobs_test

import (
	"context"
	"os"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
)

// Skip records a job that never ran: terminal at once, with the reason.
func TestSkipRecordsATerminalJob(t *testing.T) {
	r, l := setup(t, map[jobs.Kind]jobs.Handler{}, jobs.Options{})
	j, err := r.Skip(ctx, "a", []string{"t1"}, "{}", "skipped: h3 offline")
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.Skipped || got.Error != "skipped: h3 offline" || got.FinishedAt == nil || !job.Terminal(got.State) {
		t.Errorf("job = %+v", got)
	}
}

// A warning line a deploy writes per tile shows once in the job log.
func TestWarningLineOnce(t *testing.T) {
	h := func(_ context.Context, r *jobs.Run) error {
		for range 3 {
			_, _ = r.Log.Write([]byte("warning: h3 has 8% free\n"))
		}
		return nil
	}
	r, l := setup(t, map[jobs.Kind]jobs.Handler{"a": h}, jobs.Options{})
	start(t, r)
	id := enqueue(t, r, "a", "t1")
	waitState(t, l, id, job.Done)
	j, _ := l.Get(ctx, id)
	b, _ := os.ReadFile(j.LogPath)
	if string(b) != "warning: h3 has 8% free\n" {
		t.Errorf("log = %q", b)
	}
}
