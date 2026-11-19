package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A failure a later push cured leaves the window but stays on the screen,
// found by the Failed filter and marked fixed; the screen lists every
// release, filters by state and text, and scopes by org and stack.
func TestReleaseScreenKeepsFixedFailure(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	ctx := context.Background()
	p, err := w.e.Orch.SessionPrincipal(ctx, w.user)
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"org_id":"` + w.acme + `","stack_id":"` + w.shop + `"}`
	push := func(state, msg string, ago time.Duration) {
		t.Helper()
		at := time.Now().Add(-ago)
		if err := w.e.Store.Jobs.Create(ctx, store.Job{
			ID: uuid.NewString(), Kind: "push", State: state, Error: msg, Payload: payload,
			LockSet: store.StringList{"push:" + w.shop, "push:" + w.shop + ":acme/shop@main"}, CreatedAt: at, FinishedAt: &at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	push("failed", "build web, exit status 1", 3*time.Hour)
	push("done", "", time.Hour)

	win, err := w.e.Orch.ReleaseWindow(ctx, w.shop, "")
	if err != nil || win.Failed != 0 {
		t.Fatalf("window = %+v, %v: a fixed failure is not in it", win, err)
	}
	sc, err := w.e.Orch.ReleaseScreen(ctx, p, service.ScreenQuery{State: "failed"})
	if err != nil || len(sc.Rows) != 1 || !sc.Rows[0].Fixed || sc.Rows[0].Org != "acme" || sc.Rows[0].Scope != "shop" {
		t.Fatalf("failed filter = %+v, %v", sc.Rows, err)
	}
	if sc.Rows[0].Err != "build web, exit status 1" {
		t.Errorf("err = %q", sc.Rows[0].Err)
	}
	all, _ := w.e.Orch.ReleaseScreen(ctx, p, service.ScreenQuery{Scope: "acme/shop"})
	if len(all.Rows) < 3 { // two releases and the failure
		t.Errorf("rows = %d", len(all.Rows))
	}
	if n, _ := w.e.Orch.ReleaseScreen(ctx, p, service.ScreenQuery{Scope: "beta"}); len(n.Rows) != 0 {
		t.Errorf("beta rows = %+v", n.Rows)
	}
	if n, _ := w.e.Orch.ReleaseScreen(ctx, p, service.ScreenQuery{Q: "zzz-none"}); len(n.Rows) != 0 {
		t.Errorf("search rows = %+v", n.Rows)
	}
	// a viewer outside the org sees none of it
	out := w.e.User(t, "out@x.io", false)
	po, _ := w.e.Orch.SessionPrincipal(ctx, out)
	if n, err := w.e.Orch.ReleaseScreen(ctx, po, service.ScreenQuery{}); err != nil || len(n.Rows) != 0 || len(n.Scopes) != 0 {
		t.Errorf("outsider: %+v, %v", n, err)
	}
}
