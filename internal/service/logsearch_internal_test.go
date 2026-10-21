package service

import (
	"context"
	"sync"
	"testing"
	"time"

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

	for _, in := range []LogsIn{{Source: "nope"}, {Stream: "x"}, {Since: 8 * 24 * time.Hour}, {Since: -time.Second}} {
		if _, err := o.SearchLogs(ctx, sc, in); err == nil {
			t.Errorf("%+v was accepted", in)
		}
	}
}
