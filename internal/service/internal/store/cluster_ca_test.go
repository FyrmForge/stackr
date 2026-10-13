package store_test

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

func TestClusterCA(t *testing.T) {
	ctx := context.Background()
	s := servicetest.Store(t)
	ca := store.NewClusterCA(s.DB())

	if _, ok, err := ca.Get(ctx); ok || err != nil {
		t.Fatalf("empty: %v %v", ok, err)
	}
	if err := ca.Save(ctx, store.ClusterCA{CurrentFP: "aa"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ca.Get(ctx)
	if !ok || err != nil || got.CurrentFP != "aa" || got.RolloverStartedAt != nil || got.RollingOver() {
		t.Fatalf("first: %+v %v %v", got, ok, err)
	}
	next := store.ClusterCA{CurrentFP: "bb", PreviousFP: "aa", RolloverStartedAt: &t0}
	if err := ca.Save(ctx, next); err != nil {
		t.Fatal(err)
	}
	got, _, err = ca.Get(ctx)
	if err != nil || got.CurrentFP != "bb" || got.PreviousFP != "aa" || !got.RolloverStartedAt.Equal(t0) || !got.RollingOver() {
		t.Fatalf("rolling: %+v %v", got, err)
	}
	next.PreviousFP, next.RolloverFinishedAt, next.BreakGlassAt = "", &t1, &t1
	if err := ca.Save(ctx, next); err != nil {
		t.Fatal(err)
	}
	got, _, _ = ca.Get(ctx)
	if got.PreviousFP != "" || got.RollingOver() || got.BreakGlassAt == nil || !got.BreakGlassAt.Equal(t1) {
		t.Fatalf("finished: %+v", got)
	}
}
