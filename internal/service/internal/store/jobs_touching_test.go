package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ListTouching reads job_tiles (kept by triggers): any-of match, kind filter,
// newest first, limit, and a lock_set update moves the job.
func TestJobsListTouching(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := servicetest.Store(t)
	mk := func(id, kind string, age int, lock ...string) {
		mustNil(t, s.Jobs.Create(ctx, store.Job{ID: id, Kind: kind, State: "done", LockSet: lock, Payload: "{}",
			CreatedAt: t0.Add(time.Duration(age) * time.Minute)}))
	}
	mk("j1", "deploy", 1, "a")
	mk("j2", "stop", 2, "a", "b")
	mk("j3", "deploy", 3, "b", "env:e1")
	mk("j4", "deploy", 4, "c")
	mk("j5", "deploy", 5, "*")
	ids := func(js []store.Job, err error) string {
		mustNil(t, err)
		var out []string
		for _, j := range js {
			out = append(out, j.ID)
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		tiles []string
		kind  string
		limit int
		want  string
	}{
		{[]string{"a"}, "", 10, "j2,j1"},
		{[]string{"a", "b"}, "", 10, "j3,j2,j1"},
		{[]string{"a", "b"}, "deploy", 10, "j3,j1"},
		{[]string{"a", "b"}, "", 2, "j3,j2"},
		{[]string{"env:e1"}, "", 10, "j3"},
		{[]string{"*"}, "", 10, "j5"},
		{[]string{"zzz"}, "", 10, ""},
		{nil, "", 10, ""},
	} {
		if got := ids(s.Jobs.ListTouching(ctx, c.tiles, c.kind, c.limit)); got != c.want {
			t.Errorf("ListTouching(%v, %q, %d) = %s, want %s", c.tiles, c.kind, c.limit, got, c.want)
		}
	}
	j, err := s.Jobs.Get(ctx, "j1")
	mustNil(t, err)
	j.LockSet = store.StringList{"c"}
	mustNil(t, s.Jobs.Update(ctx, j))
	if got := ids(s.Jobs.ListTouching(ctx, []string{"a"}, "", 10)); got != "j2" {
		t.Errorf("after lock_set update, a = %s", got)
	}
	if got := ids(s.Jobs.ListTouching(ctx, []string{"c"}, "", 10)); got != "j4,j1" {
		t.Errorf("after lock_set update, c = %s", got)
	}
	mustNil(t, s.Jobs.Delete(ctx, "j2"))
	var n int
	mustNil(t, s.DB().Get(&n, `SELECT count(*) FROM job_tiles WHERE job_id = 'j2'`))
	if n != 0 {
		t.Errorf("job_tiles rows left after delete: %d", n)
	}
	got, err := s.Jobs.ListRecent(ctx, 2, "done")
	if ids(got, err) != "j5,j4" {
		t.Errorf("ListRecent limit 2 = %s", ids(got, err))
	}
}

// The ListTouching shape searches job_tiles by tile, not jobs by scan.
func TestJobsListTouchingPlan(t *testing.T) {
	t.Parallel()
	s := servicetest.Store(t)
	var rows []struct {
		ID      int
		Parent  int
		Notused int
		Detail  string
	}
	mustNil(t, s.DB().Select(&rows, `EXPLAIN QUERY PLAN SELECT id FROM jobs WHERE id IN
		(SELECT job_id FROM job_tiles WHERE tile_id IN (?, ?)) AND kind = ? ORDER BY created_at DESC LIMIT ?`, "a", "b", "deploy", 5))
	var plan []string
	for _, r := range rows {
		plan = append(plan, r.Detail)
	}
	p := strings.Join(plan, " | ")
	t.Log(p)
	if !strings.Contains(p, "job_tiles_tile") || strings.Contains(p, "SCAN jobs") {
		t.Errorf("plan = %s", p)
	}
}
