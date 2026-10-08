package job_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func TestViews(t *testing.T) {
	st := servicetest.Store(t)
	l := job.New(st.Jobs)
	dir := t.TempDir()

	a, _ := l.Create(ctx, "deploy", []string{"t1"}, "{}", nil, dir)
	b, _ := l.Create(ctx, "deploy", []string{"t1", "t2"}, "{}", nil, dir)
	c, _ := l.Create(ctx, "restart", []string{"t2"}, "{}", nil, dir)
	if err := l.Finish(ctx, a, job.Done, ""); err != nil {
		t.Fatal(err)
	}
	if err := l.Finish(ctx, b, job.Failed, "boom"); err != nil {
		t.Fatal(err)
	}

	if h, _ := l.History(ctx, []string{"t1"}, 10); len(h) != 2 || h[0].ID != b.ID {
		t.Errorf("t1 history = %v", h)
	}
	if h, _ := l.History(ctx, []string{"t1", "t2"}, 2); len(h) != 2 || h[0].ID != c.ID {
		t.Errorf("env history = %v", h)
	}
	if last, ok, _ := l.Last(ctx, "t1"); !ok || last.ID != b.ID {
		t.Errorf("last = %v %v", last.ID, ok)
	}
	if done, ok, _ := l.LastDone(ctx, "t1", "deploy"); !ok || done.ID != a.ID {
		t.Errorf("last done = %v %v", done.ID, ok)
	}
	if _, ok, _ := l.LastDone(ctx, "t2", "deploy"); ok {
		t.Error("t2 has no finished deploy")
	}
	if _, ok, _ := l.Last(ctx, "t9"); ok {
		t.Error("a tile with no jobs has a last job")
	}
}

func TestPoll(t *testing.T) {
	st := servicetest.Store(t)
	l := job.New(st.Jobs)
	j, _ := l.Create(ctx, "deploy", []string{"t1"}, "{}", nil, t.TempDir())

	_, lg, err := l.Poll(ctx, j.ID, 0)
	if err != nil || len(lg.Chunk) != 0 || lg.End {
		t.Fatalf("no log yet = %+v %v", lg, err)
	}
	if err := os.WriteFile(j.LogPath, []byte("step 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, lg, _ = l.Poll(ctx, j.ID, 0)
	if string(lg.Chunk) != "step 1\n" || lg.End {
		t.Errorf("running = %+v", lg)
	}
	f, _ := os.OpenFile(filepath.Clean(j.LogPath), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(strings.Repeat("x", job.MaxChunk))
	_ = f.Close()
	if err := l.Finish(ctx, j, job.Done, ""); err != nil {
		t.Fatal(err)
	}
	_, lg, _ = l.Poll(ctx, j.ID, lg.Next)
	if len(lg.Chunk) != job.MaxChunk || lg.End {
		t.Errorf("full chunk = %d bytes, end %v", len(lg.Chunk), lg.End)
	}
	_, lg, _ = l.Poll(ctx, j.ID, lg.Next)
	if len(lg.Chunk) != 0 || !lg.End {
		t.Errorf("tail = %+v", lg)
	}
}

// Requeue only brings back a row that is still waiting: a job cancelled or
// superseded after the caller read it stays that way.
func TestRequeueOnlyWaiting(t *testing.T) {
	st := servicetest.Store(t)
	l := job.New(st.Jobs)
	j, _ := l.Create(ctx, "deploy", []string{"t1"}, "{}", nil, t.TempDir())
	j, _ = l.Start(ctx, j)
	if err := l.Park(ctx, j, "p"); err != nil {
		t.Fatal(err)
	}
	stale, _ := l.Get(ctx, j.ID)
	if err := l.Finish(ctx, stale, job.Cancelled, ""); err != nil {
		t.Fatal(err)
	}
	if err := l.Requeue(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.Get(ctx, j.ID); got.State != job.Cancelled {
		t.Errorf("state = %s; want cancelled", got.State)
	}
}

// cancelAfterGet finishes the row right after the leaf reads it, and right
// before it requeues: the cancel that lands in Requeue's window.
type cancelAfterGet struct{ store.JobStore }

func (c cancelAfterGet) cancel(ctx context.Context, id string) error {
	j, err := c.JobStore.Get(ctx, id)
	if err != nil || j.State != job.Waiting {
		return err
	}
	j.State = job.Cancelled
	return c.Update(ctx, j)
}

func (c cancelAfterGet) Get(ctx context.Context, id string) (store.Job, error) {
	j, err := c.JobStore.Get(ctx, id)
	if err == nil {
		err = c.cancel(ctx, id)
	}
	return j, err
}

func (c cancelAfterGet) RequeueIfWaiting(ctx context.Context, id string) error {
	if err := c.cancel(ctx, id); err != nil {
		return err
	}
	return c.JobStore.RequeueIfWaiting(ctx, id)
}

// A cancel that lands while Requeue runs is not undone by it.
func TestRequeueRaceWithCancel(t *testing.T) {
	st := servicetest.Store(t)
	l := job.New(st.Jobs)
	j, _ := l.Create(ctx, "deploy", []string{"t1"}, "{}", nil, t.TempDir())
	j, _ = l.Start(ctx, j)
	if err := l.Park(ctx, j, "p"); err != nil {
		t.Fatal(err)
	}
	if err := job.New(cancelAfterGet{st.Jobs}).Requeue(ctx, j); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.Get(ctx, j.ID); got.State != job.Cancelled {
		t.Errorf("state = %s; want cancelled", got.State)
	}
}

// Park only takes a row that is still running: one cancelled meanwhile stays so.
func TestParkOnlyRunning(t *testing.T) {
	st := servicetest.Store(t)
	l := job.New(st.Jobs)
	j, _ := l.Create(ctx, "deploy", []string{"t1"}, "{}", nil, t.TempDir())
	j, _ = l.Start(ctx, j)
	if err := l.Finish(ctx, j, job.Cancelled, ""); err != nil {
		t.Fatal(err)
	}
	if err := l.Park(ctx, j, "p"); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.Get(ctx, j.ID); got.State != job.Cancelled {
		t.Errorf("state = %s; want cancelled", got.State)
	}
}
