package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/logs"
)

// scopedLogs is a store that applies the org filter like the real one and
// remembers the last filter and window it was asked.
type scopedLogs struct {
	memLogs
	mu       sync.Mutex
	lines    []logs.Line
	got      logs.Filter
	from, to time.Time
}

func (s *scopedLogs) Query(_ context.Context, f logs.Filter, from, to time.Time, _ int) ([]logs.Line, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got, s.from, s.to = f, from, to
	var out []logs.Line
	for _, l := range s.lines {
		if l.OrgID == f.Org {
			out = append(out, l)
		}
	}
	return out, nil
}

// A search is always filtered by the scope's org, whatever the request
// carries: org B's lines never come back for org A. The scope's ids and the
// request's filters map onto the store filter, and the window follows Since
// and Before.
func TestSearchLogs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newWorld(t)
	o := w.orch
	tl := w.tile(t, "api", false)
	st := &scopedLogs{lines: []logs.Line{
		{OrgID: w.org, Msg: "mine"},
		{OrgID: "org-b", Msg: "theirs"},
	}}
	o.logship.store = st
	if _, err := o.SearchLogs(ctx, Scope{Org: &Org{ID: w.org}}, LogsIn{}); err == nil {
		t.Fatal("a search with logs off was answered")
	}
	o.logship.on.Store(true)

	sc, err := o.Resolve(ctx, "acme", "shop", "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	// a tile id of another org in the request narrows, it never widens
	got, err := o.SearchLogs(ctx, sc, LogsIn{Tile: "org-b-tile"})
	if err == nil {
		t.Fatalf("a foreign tile was accepted: %v", got)
	}
	got, err = o.SearchLogs(ctx, Scope{Org: sc.Org}, LogsIn{Text: "x"})
	must(t, err)
	if len(got) != 1 || got[0].Msg != "mine" || st.got.Org != w.org {
		t.Fatalf("lines %+v filter %+v", got, st.got)
	}

	before := time.Now().Add(-time.Minute)
	_, err = o.SearchLogs(ctx, sc, LogsIn{
		Tile: tl.Slug, Kind: "service", Source: "access", Stream: "stdout", Run: "r1", Text: "boom",
		Since: 6 * time.Hour, Before: before,
	})
	must(t, err)
	want := logs.Filter{
		Org: w.org, Stack: w.stack, Env: w.env, Tile: tl.ID,
		Kind: "service", Source: "access", Stream: "stdout", Run: "r1", Text: "boom",
	}
	if st.got != want {
		t.Errorf("filter %+v, want %+v", st.got, want)
	}
	if !st.to.Before(before) || time.Since(st.from) < 5*time.Hour {
		t.Errorf("window %v..%v", st.from, st.to)
	}

	// a tile scope pins the tile, whatever ?tile says
	sc.Tile = &tl
	_, err = o.SearchLogs(ctx, sc, LogsIn{Tile: "other"})
	must(t, err)
	if st.got.Tile != tl.ID {
		t.Errorf("tile %q", st.got.Tile)
	}

	for _, in := range []LogsIn{{Kind: "x"}, {Source: "nope"}, {Stream: "x"}, {Since: 8 * 24 * time.Hour}, {Since: -time.Second}} {
		if _, err := o.SearchLogs(ctx, sc, in); err == nil {
			t.Errorf("%+v was accepted", in)
		}
	}
	// a search reaches as far back as the logs are kept
	must(t, o.settings.Set(ctx, "observe_logs_retention", "30"))
	if _, err := o.SearchLogs(ctx, sc, LogsIn{Since: 30 * 24 * time.Hour}); err != nil {
		t.Errorf("30d under a 30 day retention = %v", err)
	}
	// ... and never less than the viewer's 7 days
	must(t, o.settings.Set(ctx, "observe_logs_retention", "2"))
	if _, err := o.SearchLogs(ctx, sc, LogsIn{Since: 7 * 24 * time.Hour}); err != nil {
		t.Errorf("7d under a 2 day retention = %v", err)
	}
}

// delLogs records the filters it was asked to delete.
type delLogs struct {
	memLogs
	got []logs.Filter
}

func (d *delLogs) Delete(_ context.Context, f logs.Filter) error {
	d.got = append(d.got, f)
	return nil
}

// A purge deletes the scope's own org and env, and nothing without an env.
func TestPurgeLogs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newWorld(t)
	o := w.orch
	st := &delLogs{}
	o.logship.store = st
	sc, err := o.Resolve(ctx, "acme", "shop", "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := o.PurgeLogs(ctx, sc); err == nil {
		t.Fatal("a purge with logs off ran")
	}
	o.logship.on.Store(true)
	if err := o.PurgeLogs(ctx, Scope{Org: sc.Org}); err == nil || len(st.got) != 0 {
		t.Fatalf("a purge without an env ran: %v %v", err, st.got)
	}
	if err := o.PurgeLogs(ctx, sc); err != nil {
		t.Fatal(err)
	}
	if len(st.got) != 1 || st.got[0].Org != sc.Org.ID || st.got[0].Env != sc.Env.ID {
		t.Errorf("deleted %+v", st.got)
	}
}

func TestObserveUnreachable(t *testing.T) {
	t.Parallel()
	dial := &url.Error{Op: "Post", URL: "http://observe/x", Err: &net.OpError{Op: "dial", Net: "unix", Err: os.ErrNotExist}}
	for _, c := range []struct {
		err  error
		want bool
	}{
		{dial, true},
		{fmt.Errorf("w: %w", syscall.ECONNREFUSED), true},
		{fmt.Errorf("w: %w", syscall.ENOENT), true},
		{context.DeadlineExceeded, true},
		{fmt.Errorf("w: %w", io.EOF), true},
		{io.ErrUnexpectedEOF, true},
		{errors.New("victorialogs /x: 400 Bad Request"), false},
		{context.Canceled, false},
		{nil, false},
	} {
		if got := observeUnreachable(c.err); got != c.want {
			t.Errorf("observeUnreachable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

type downLogs struct{ memLogs }

func (*downLogs) Query(context.Context, logs.Filter, time.Time, time.Time, int) ([]logs.Line, error) {
	return nil, &net.OpError{Op: "dial", Net: "unix", Err: syscall.ENOENT}
}

// A store that cannot be reached is ErrObserveDown (a 503 on the API), not a
// bare dial error.
func TestSearchLogsObserveDown(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	o := w.orch
	o.logship.store = &downLogs{}
	o.logship.on.Store(true)
	_, err := o.SearchLogs(context.Background(), Scope{Org: &Org{ID: w.org}}, LogsIn{})
	if !errors.Is(err, ErrObserveDown) || !errors.Is(err, errs.ErrBusy) {
		t.Fatalf("err = %v", err)
	}
}
