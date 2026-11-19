package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// The window lists a running push first, a failed push next, then what
// waits, then what runs; the counts say waiting and failed apart.
func TestReleaseWindowRows(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	ctx := context.Background()
	now := time.Now()
	job := func(state, payload string, lock ...string) {
		t.Helper()
		fin := now
		if err := w.e.Store.Jobs.Create(ctx, store.Job{
			ID: uuid.NewString(), Kind: "push", State: state, Payload: payload, Error: "build immich-ml, exit status 1",
			LockSet: store.StringList(lock), CreatedAt: now, FinishedAt: &fin,
		}); err != nil {
			t.Fatal(err)
		}
	}
	job("failed", "{}", "push:"+w.shop, "push:"+w.shop+":acme/shop@main")
	job("running", `{"org_id":"`+w.acme+`","stack_id":"`+w.shop+`","event":{"Commit":"f9cdc50"}}`, "push:"+w.shop)

	win, err := w.e.Orch.ReleaseWindow(ctx, w.shop, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range win.Rows {
		got = append(got, r.Kind+" "+r.State)
	}
	want := []string{"building ", "failed failed", "release waiting", "release running"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	}
	if win.Waiting != 1 || win.Failed != 1 || win.Rows[0].Commit != "f9cdc50" || win.Rows[2].Number != 2 {
		t.Errorf("window = %+v", win)
	}
	// seen from dev, which runs the newest, nothing waits
	if win, err = w.e.Orch.ReleaseWindow(ctx, w.shop, w.dev); err != nil || win.Waiting != 0 {
		t.Errorf("dev: %+v, %v", win, err)
	}
	// another stack hears of none of it
	other, err := w.e.Orch.CreateStack(ctx, w.acme, "other", "")
	if err != nil {
		t.Fatal(err)
	}
	if win, err = w.e.Orch.ReleaseWindow(ctx, other.ID, ""); err != nil || len(win.Rows) != 0 || win.Failed != 0 {
		t.Errorf("other stack: %+v, %v", win, err)
	}
}

// A one-env stack shows what waits for its only env, the newest of it, and
// that the later release leaves the earlier one out; "roll back" is said only
// of an ancestor.
func TestReleaseWindowWaitingAndNewest(t *testing.T) {
	t.Parallel()
	e := servicetest.New(t)
	ctx := context.Background()
	org := e.Org(t, "acme")
	st, err := e.Orch.CreateStack(ctx, org, "immich", "")
	if err != nil {
		t.Fatal(err)
	}
	prod, err := e.Orch.CreateEnv(ctx, st.ID, "prod", service.EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	rel := func(n int, builtOn, line, commit, digest string) string {
		t.Helper()
		id := uuid.NewString()
		r := store.Release{ID: id, StackID: st.ID, Number: n, CreatedAt: time.Now(), CreatedBy: "push"}
		if builtOn != "" {
			r.BuiltOn, r.Line = &builtOn, &line
		}
		if err := e.Store.Releases.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		for slug, p := range map[string][2]string{"_config": {commit, ""}, "server": {"", digest}} {
			if err := e.Store.ReleaseTiles.Create(ctx, store.ReleaseTile{ID: uuid.NewString(), ReleaseID: id, Slug: slug, CommitSHA: p[0], Digest: p[1]}); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	d := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	r1 := rel(1, "", "", "c1", d("a"))
	r3 := rel(3, r1, "branch:main", "c3", d("a"))  // newer config, same image
	r4 := rel(4, r1, "env:"+prod.ID, "c1", d("b")) // image watch, old config
	row, _ := e.Store.Environments.Get(ctx, prod.ID)
	row.ReleaseID = &r1
	if err := e.Store.Environments.Update(ctx, row); err != nil {
		t.Fatal(err)
	}

	win, err := e.Orch.ReleaseWindow(ctx, st.ID, prod.ID)
	if err != nil {
		t.Fatal(err)
	}
	by := map[int]service.WindowRow{}
	for _, r := range win.Rows {
		by[r.Number] = r
	}
	if win.Waiting != 2 || by[3].State != "waiting" || by[4].State != "waiting" || by[1].State != "running" {
		t.Fatalf("window = %+v", win)
	}
	if !by[4].Newest || by[3].Newest || by[4].Lacks != 3 || by[3].Lacks != 0 {
		t.Errorf("newest/lacks: 3 = %+v, 4 = %+v", by[3], by[4])
	}
	if e.Orch.IsRollback(ctx, prod.ID, r3) || e.Orch.IsRollback(ctx, prod.ID, r4) {
		t.Error("a release newer than what runs is a roll back")
	}
	// the env takes 4: 1 is an ancestor, 3 is a sibling it never held
	row.ReleaseID = &r4
	if err := e.Store.Environments.Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if !e.Orch.IsRollback(ctx, prod.ID, r1) {
		t.Error("an ancestor of what runs is a roll back")
	}
	if e.Orch.IsRollback(ctx, prod.ID, r3) {
		t.Error("release 3 was never held and is not an ancestor: a plan, not a roll back")
	}
}
