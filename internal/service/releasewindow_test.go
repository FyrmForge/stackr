package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
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
