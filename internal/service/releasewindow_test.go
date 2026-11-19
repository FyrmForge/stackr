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

// The org window lists the org file's waiting plan with the stacks' open
// releases (all in one list, grouped by need), counts both and the plans
// alone; the server window lists the server file's plans, a plan that
// failed counted apart.
func TestOrgAndServerWindow(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	ctx := context.Background()
	now := time.Now()
	// the stored summary is counts; the window names the items from the plan's changes
	const named = `{"changes":[{"kind":"route-delete","tile":"auth.vulpe.dev","optional":true}]}`
	plan := func(org, status, summary string, age time.Duration) {
		t.Helper()
		var err error
		if org != "" {
			err = w.e.Store.OrgPlans.Create(ctx, store.OrgPlan{ID: uuid.NewString(), OrgID: org, Status: status, Summary: summary, Plan: named, Ticked: []string{}, CreatedAt: now.Add(-age)})
		} else {
			err = w.e.Store.ServerPlans.Create(ctx, store.ServerPlan{ID: uuid.NewString(), Source: "repo", Status: status, Summary: summary, Plan: named, Ticked: []string{}, CreatedAt: now.Add(-age)})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	plan(w.acme, "applied", "2 applied", 2*time.Hour)
	plan(w.acme, "pending", "1 to add: stack dashy", time.Minute)
	win, err := w.e.Orch.OrgReleaseWindow(ctx, w.acme, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range win.Rows {
		got = append(got, r.Kind+" "+r.State)
	}
	// waiting plan and waiting release first (the plan, newest first, ahead), then what runs, then the applied plan
	if len(got) < 3 || got[0] != "plan waiting" || got[len(got)-1] != "plan applied" || win.OrgWaiting != 1 || win.Waiting != 2 {
		t.Errorf("org rows = %v, window %+v", got, win)
	}
	if st := win.Stacks[w.shop]; st.Slug == "" || len(st.Envs) == 0 {
		t.Errorf("stacks = %+v", win.Stacks)
	}
	if win, _ = w.e.Orch.OrgReleaseWindow(ctx, w.acme, false); win.OrgWaiting != 0 || win.Waiting != 1 {
		t.Errorf("a viewer who cannot read plans: %+v", win)
	}

	if sw, err := w.e.Orch.ServerReleaseWindow(ctx); err != nil || sw.Bound || len(sw.Rows) != 0 {
		t.Errorf("no file: %+v, %v", sw, err)
	}
	plan("", "error", "", 3*time.Hour)
	plan("", "applied", "2 applied: routes nas", 2*time.Hour)
	plan("", "pending", "1 removal to review", time.Minute)
	sw, err := w.e.Orch.ServerReleaseWindow(ctx)
	if err != nil || !sw.Bound || sw.Waiting != 1 || sw.Failed != 0 || len(sw.Rows) != 2 || sw.Rows[0].Message != "1 removal to review: route auth.vulpe.dev" {
		t.Errorf("server window = %+v, %v", sw, err)
	}
	plan("", "error", "", 0)
	if sw, _ = w.e.Orch.ServerReleaseWindow(ctx); sw.Failed != 1 || sw.Rows[0].State != service.StateFailed {
		t.Errorf("a failed newest plan: %+v", sw)
	}
}

// An image watch release has no commit message: its row reads what moved
// against the release it was built on, "immich-server 3.4.1 to 3.4.2".
func TestReleaseWindowImageWatchHeadline(t *testing.T) {
	t.Parallel()
	e := servicetest.New(t)
	ctx := context.Background()
	st, err := e.Orch.CreateStack(ctx, e.Org(t, "acme"), "immich", "")
	if err != nil {
		t.Fatal(err)
	}
	rel := func(n int, builtOn, by, tag string) {
		t.Helper()
		id := uuid.NewString()
		r := store.Release{ID: id, StackID: st.ID, Number: n, CreatedAt: time.Now(), CreatedBy: by}
		if builtOn != "" {
			line := "branch:main"
			r.BuiltOn, r.Line = &builtOn, &line
		}
		if err := e.Store.Releases.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := e.Store.ReleaseTiles.Create(ctx, store.ReleaseTile{ID: uuid.NewString(), ReleaseID: id, Slug: "immich-server", Repo: "ghcr.io/immich-app/immich-server:" + tag, Digest: "sha256:" + tag}); err != nil {
			t.Fatal(err)
		}
	}
	rel(1, "", "push", "3.4.1")
	var first store.Release
	if rs, _ := e.Store.Releases.ListByStack(ctx, st.ID); len(rs) == 1 {
		first = rs[0]
	}
	rel(2, first.ID, "image-watch", "3.4.2")
	win, err := e.Orch.ReleaseWindow(ctx, st.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, r := range win.Rows {
		if r.Number == 2 {
			seen = true
		}
		if r.Number == 2 && r.Message != "immich-server 3.4.1 to 3.4.2" {
			t.Errorf("image watch headline = %q", r.Message)
		}
		if r.Number == 1 && r.Message != "" {
			t.Errorf("a push release without a message grew %q", r.Message)
		}
	}
	if !seen {
		t.Errorf("release 2 is not in the window: %+v", win.Rows)
	}
}
