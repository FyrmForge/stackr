package store_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

func nodes(rs []store.ExitRung) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.TargetID)
	}
	return out
}

func TestExitLadders(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()
	l := store.NewExitLadders(s.DB())

	if err := s.Orgs.Create(ctx, store.Org{ID: "o1", Name: "O", Slug: "o", Settings: "{}", EnvColors: "{}", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := l.Set(ctx, "org", "o1", []string{"n2", "n3", "n1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Set(ctx, "env", "e1", []string{"n3"}); err != nil {
		t.Fatal(err)
	}
	got, err := l.List(ctx, "org", "o1")
	if err != nil || !reflect.DeepEqual(nodes(got), []string{"n2", "n3", "n1"}) || got[2].Position != 2 {
		t.Fatalf("list = %+v, %v", got, err)
	}
	// Replace shrinks and reorders.
	if err := l.Set(ctx, "org", "o1", []string{"n3", "n2"}); err != nil {
		t.Fatal(err)
	}
	if got, _ = l.List(ctx, "org", "o1"); !reflect.DeepEqual(nodes(got), []string{"n3", "n2"}) {
		t.Fatalf("after replace = %+v", got)
	}
	by, err := l.ListByTarget(ctx, "n3")
	if err != nil || len(by) != 2 {
		t.Fatalf("by target = %+v, %v", by, err)
	}
	// A node twice in one ladder is refused, and the old ladder stays unusable
	// only if the caller ignores the error; Set runs in a tx in real use.
	if err := l.Set(ctx, "org", "o1", []string{"n1", "n1"}); err == nil {
		t.Fatal("duplicate node accepted")
	}
	if err := l.Set(ctx, "bogus", "x", []string{"n1"}); err == nil {
		t.Fatal("bad scope kind accepted")
	}
	// Clearing and cascade.
	if err := l.Set(ctx, "env", "e1", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = l.List(ctx, "env", "e1"); len(got) != 0 {
		t.Fatalf("cleared = %+v", got)
	}
	if err := s.Orgs.Delete(ctx, "o1"); err != nil {
		t.Fatal(err)
	}
	if got, _ = l.List(ctx, "org", "o1"); len(got) != 0 {
		t.Fatalf("org delete left %+v", got)
	}
}
