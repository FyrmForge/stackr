package tier_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tier"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func seedOrg(t *testing.T, st *store.Store) string {
	t.Helper()
	id := uuid.NewString()
	if err := st.Orgs.Create(ctx, store.Org{
		ID: id, Name: id, Slug: id[:8], EnvColors: "{}", Settings: "{}", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func slugs(t *testing.T, l *tier.Leaf, org string) []string {
	t.Helper()
	ts, err := l.List(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range ts {
		out = append(out, x.Slug)
	}
	return out
}

func TestLadder(t *testing.T) {
	st := servicetest.Store(t)
	l := tier.New(st.Tiers)
	org, other := seedOrg(t, st), seedOrg(t, st)

	for _, bad := range []string{"", "Dev", "a b", "pr", "order", "params", "all"} {
		if _, err := l.Create(ctx, org, bad); err == nil {
			t.Errorf("create %q accepted", bad)
		}
	}
	for _, s := range []string{"dev", "staging", "prod"} {
		if x, err := l.Create(ctx, org, s); err != nil || !x.Locked {
			t.Fatalf("create %s = %+v, %v; want a locked tier", s, x, err)
		}
	}
	if _, err := l.Create(ctx, org, "dev"); err == nil {
		t.Error("duplicate slug accepted")
	}
	if _, err := l.Create(ctx, other, "dev"); err != nil {
		t.Errorf("same slug in another org: %v", err)
	}
	if got := slugs(t, l, org); len(got) != 3 || got[0] != "dev" || got[2] != "prod" {
		t.Fatalf("ladder = %v", got)
	}

	for _, bad := range [][]string{{"dev", "prod"}, {"dev", "dev", "prod"}, {"dev", "staging", "prod", "x"}, {"dev", "staging", "nope"}} {
		if err := l.Reorder(ctx, org, bad); err == nil {
			t.Errorf("reorder %v accepted", bad)
		}
	}
	if err := l.Reorder(ctx, org, []string{"prod", "dev", "staging"}); err != nil {
		t.Fatal(err)
	}
	if got := slugs(t, l, org); got[0] != "prod" || got[2] != "staging" {
		t.Errorf("reordered = %v", got)
	}

	x, _ := l.GetBySlug(ctx, org, "staging")
	if _, err := l.Rename(ctx, x, "prod"); err == nil {
		t.Error("rename onto a taken slug accepted")
	}
	if _, err := l.Rename(ctx, x, "pr"); err == nil {
		t.Error("rename to pr accepted")
	}
	x, err := l.Rename(ctx, x, "stage")
	if err != nil || x.Slug != "stage" || x.Position != 2 {
		t.Fatalf("rename = %+v, %v", x, err)
	}
	if x, err = l.SetLocked(ctx, x, false); err != nil || x.Locked {
		t.Fatalf("unlock = %+v, %v", x, err)
	}
	if got, _ := l.GetBySlug(ctx, org, "stage"); got.Locked {
		t.Error("lock not stored")
	}

	if got, ok, err := l.Of(ctx, org, "stage"); err != nil || !ok || got.ID != x.ID {
		t.Errorf("Of stage = %+v, %v, %v", got, ok, err)
	}
	if _, ok, err := l.Of(ctx, org, "demo"); err != nil || ok {
		t.Errorf("Of demo = %v, %v; want off-tier", ok, err)
	}
	if _, ok, _ := l.Of(ctx, other, "stage"); ok {
		t.Error("Of crossed orgs")
	}

	if err := l.Delete(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Get(ctx, x.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("get after delete: %v", err)
	}
}
