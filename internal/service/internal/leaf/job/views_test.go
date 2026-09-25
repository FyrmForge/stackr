package job_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
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
