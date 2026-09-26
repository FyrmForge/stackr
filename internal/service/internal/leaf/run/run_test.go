package run_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// setup makes a cron tile and a second tile to own foreign runs.
func setup(t *testing.T) (*run.Leaf, string, string) {
	st := servicetest.Store(t)
	org, stack, env := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{
		ID:        org,
		Name:      org,
		Slug:      org[:8],
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, st.Stacks.Create(ctx, store.Stack{
		ID:        stack,
		OrgID:     org,
		Name:      "s",
		Slug:      "s",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, st.Environments.Create(ctx, store.Environment{
		ID:         env,
		StackID:    stack,
		Name:       "dev",
		Slug:       "dev",
		Type:       "static",
		Settings:   "{}",
		Network:    "n",
		FromKind:   "branch",
		FromBranch: "main",
		CreatedAt:  now,
	}))
	var ids []string
	for _, s := range []string{"sweep", "other"} {
		id := uuid.NewString()
		must(t, st.Tiles.Create(ctx, store.Tile{
			ID:             id,
			StackID:        stack,
			EnvironmentID:  env,
			Name:           s,
			Slug:           s,
			Kind:           "cron",
			ImageRef:       "alpine:3",
			Schedule:       "* * * * *",
			TimeoutMinutes: 30,
			UpdatePolicy:   "manual",
			CreatedAt:      now,
			UpdatedAt:      now,
		}))
		ids = append(ids, id)
	}
	return run.New(st.Runs, t.TempDir()), ids[0], ids[1]
}

func TestRoundTrip(t *testing.T) {
	l, tile, other := setup(t)
	r, err := l.Start(ctx, tile, nil, run.Manual)
	must(t, err)
	if a, ok, _ := l.Active(ctx, tile); !ok || a.ID != r.ID {
		t.Fatalf("active = %+v %v", a, ok)
	}
	r, err = l.SetJob(ctx, r.ID, "job-1")
	must(t, err)
	r, err = l.Begin(ctx, r.ID)
	must(t, err)
	if r.Status != run.Running || r.StartedAt == nil {
		t.Fatalf("begin = %+v", r)
	}
	code := 3
	r, err = l.Finish(ctx, r.ID, &code, run.Failed, "")
	must(t, err)
	if again, _ := l.Finish(ctx, r.ID, nil, run.Cancelled, "late stop"); again.Status != run.Failed {
		t.Errorf("finish of a closed run moved it to %s", again.Status)
	}
	got, err := l.Get(ctx, tile, r.ID)
	if err != nil || got.JobID != "job-1" || *got.ExitCode != 3 || got.FinishedAt == nil || got.Trigger != run.Manual {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := l.Get(ctx, other, r.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("another tile's run = %v, want not found", err)
	}
	if last, ok, _ := l.Last(ctx, tile); !ok || last.ID != r.ID {
		t.Errorf("last = %+v", last)
	}
	if _, ok, _ := l.Active(ctx, tile); ok {
		t.Error("a finished run is still active")
	}
}

func TestPrune(t *testing.T) {
	l, tile, _ := setup(t)
	var active, first store.Run
	for i := range run.Keep + 2 {
		r, err := l.Start(ctx, tile, nil, run.Schedule)
		must(t, err)
		switch i {
		case 0: // the oldest is still going: never pruned
			active, err = l.Begin(ctx, r.ID)
			must(t, err)
		case 1:
			first = r
			g, err := l.OpenLog(r)
			must(t, err)
			_, _ = g.Write([]byte("hello\n"))
			must(t, g.Close())
			fallthrough
		default:
			_, err = l.Finish(ctx, r.ID, nil, run.OK, "")
			must(t, err)
		}
		time.Sleep(time.Millisecond) // created_at orders the list
	}
	rs, err := l.List(ctx, tile, 0)
	must(t, err)
	if len(rs) != run.Keep+1 || rs[len(rs)-1].ID != active.ID {
		t.Fatalf("kept %d runs, oldest %s; want %d and the running one %s", len(rs), rs[len(rs)-1].ID, run.Keep+1, active.ID)
	}
	for _, r := range rs {
		if r.ID == first.ID {
			t.Fatal("the oldest finished run was kept")
		}
	}
	if _, err := os.Stat(l.LogPath(first)); !os.IsNotExist(err) {
		t.Errorf("pruned run's log: %v", err)
	}
	if rs, _ := l.List(ctx, tile, 5); len(rs) != 5 {
		t.Errorf("limit 5 = %d", len(rs))
	}
}

func TestLogCap(t *testing.T) {
	l, tile, _ := setup(t)
	r, err := l.Start(ctx, tile, nil, run.Manual)
	must(t, err)
	g, err := l.OpenLog(r)
	must(t, err)
	line := strings.Repeat("x", 1023) + "\n"
	for range 3 * 1024 { // 3 MiB
		_, err := g.Write([]byte(line))
		must(t, err)
	}
	_, _ = g.Write([]byte("the end\n"))
	must(t, g.Close())
	if sz := fileSize(t, l.LogPath(r)); sz != run.LogCap {
		t.Errorf("log size = %d, want %d", sz, run.LogCap)
	}
	if tail, err := l.Tail(r, 1); err != nil || tail != "the end\n" {
		t.Errorf("tail = %q, %v", tail, err)
	}
}

func TestFollow(t *testing.T) {
	l, tile, _ := setup(t)
	r, _ := l.Start(ctx, tile, nil, run.Manual)
	g, err := l.OpenLog(r)
	must(t, err)
	_, _ = g.Write([]byte("one\n"))
	done := make(chan struct{})
	lines, stop := l.Follow(r, 10, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	defer stop()
	_, _ = g.Write([]byte("two\n"))
	must(t, g.Close())
	close(done)
	var got []string
	for s := range lines {
		got = append(got, s)
	}
	if strings.Join(got, ",") != "one,two" {
		t.Errorf("follow = %v", got)
	}
}

func TestNext(t *testing.T) {
	from := time.Date(2026, 9, 24, 10, 7, 0, 0, time.UTC)
	if n, err := run.Next("*/15 * * * *", from); err != nil || !n.Equal(from.Add(8*time.Minute)) {
		t.Errorf("next = %v, %v", n, err)
	}
	if _, err := run.Next("nope", from); err == nil {
		t.Error("bad expr parsed")
	}
}

func fileSize(t *testing.T, p string) int64 {
	st, err := os.Stat(p)
	must(t, err)
	return st.Size()
}
