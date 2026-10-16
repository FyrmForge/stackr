package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

func TestPlacements(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()

	def, err := s.Placements.GetByName(ctx, "default")
	if err != nil || def.NodeID != "" || !def.OpenToAll {
		t.Fatalf("migrated default = %+v, %v; want open, no node before boot", def, err)
	}
	p := store.Placement{ID: "p1", Name: "eu-prod", NodeID: "n1", OpenToAll: true, CreatedAt: t0}
	if err := s.Placements.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.Placements.Create(ctx, store.Placement{ID: "p2", Name: "eu-prod", CreatedAt: t0}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if got, err := s.Placements.Get(ctx, "p1"); err != nil || !got.CreatedAt.Equal(t0) || got.Name != "eu-prod" || got.NodeID != "n1" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if err := s.Placements.SetNode(ctx, "p1", "n2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Placements.SetOpen(ctx, "p1", false); err != nil {
		t.Fatal(err)
	}
	if on, _ := s.Placements.ListByNode(ctx, "n2"); len(on) != 1 || on[0].OpenToAll {
		t.Fatalf("by node = %+v", on)
	}
	if all, _ := s.Placements.List(ctx); len(all) != 2 {
		t.Fatalf("list = %+v", all)
	}
	if err := s.Placements.SetNode(ctx, "nope", "n2"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("set node of a missing placement = %v", err)
	}

	// Access list.
	for _, id := range []string{"o1", "o2"} {
		if err := s.Orgs.Create(ctx, store.Org{ID: id, Name: id, Slug: id, Settings: "{}", EnvColors: "{}", CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PlacementOrgs.Set(ctx, "p1", []string{"o2", "o1"}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.PlacementOrgs.OrgIDs(ctx, "p1"); !reflect.DeepEqual(ids, []string{"o1", "o2"}) {
		t.Errorf("orgs = %v", ids)
	}
	if err := s.PlacementOrgs.Set(ctx, "p1", []string{"o2"}); err != nil {
		t.Fatal(err)
	}
	if has, _ := s.PlacementOrgs.Has(ctx, "p1", "o1"); has {
		t.Error("o1 still listed after replace")
	}

	// A pinned placement cannot go; an unpinned one can.
	og, _ := s.Orgs.Get(ctx, "o1")
	og.PlacementID = ptr("p1")
	if err := s.Orgs.Update(ctx, og); err != nil {
		t.Fatal(err)
	}
	if err := s.Placements.Delete(ctx, "p1"); err == nil {
		t.Fatal("deleted a pinned placement")
	}
	og.PlacementID = nil
	if err := s.Orgs.Update(ctx, og); err != nil {
		t.Fatal(err)
	}

	// Moves: the newest is Latest; SetState finishes one.
	for _, m := range []store.PlacementMove{
		{ID: "m1", PlacementID: "p1", FromNode: "n1", ToNode: "n2", State: "failed", By: "u1", StartedAt: t0},
		{ID: "m2", PlacementID: "p1", FromNode: "n1", ToNode: "n2", State: "running", By: "u1", StartedAt: t1},
	} {
		if err := s.PlacementMoves.Create(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if m, err := s.PlacementMoves.Latest(ctx, "p1"); err != nil || m.ID != "m2" {
		t.Fatalf("latest = %+v, %v", m, err)
	}
	if err := s.PlacementMoves.SetState(ctx, "m2", "done", "", &t1); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.PlacementMoves.Get(ctx, "m2"); m.State != "done" || m.FinishedAt == nil || !m.FinishedAt.Equal(t1) {
		t.Errorf("after set state = %+v", m)
	}
	if _, err := s.PlacementMoves.Latest(ctx, "p2"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("latest of none = %v", err)
	}

	if err := s.Placements.Delete(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.PlacementOrgs.OrgIDs(ctx, "p1"); len(n) != 0 {
		t.Errorf("access rows outlived the placement: %v", n)
	}
}

// SetPlacement is the one writer of an env's placement: a whole-row Update
// from a stale copy leaves it.
func TestEnvPlacement(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Orgs.Create(ctx, store.Org{ID: "o1", Name: "O", Slug: "o", Settings: "{}", EnvColors: "{}", CreatedAt: t0}))
	must(s.Stacks.Create(ctx, store.Stack{ID: "s1", OrgID: "o1", Name: "S", Slug: "s", Settings: "{}", CreatedAt: t0}))
	e := store.Environment{ID: "e1", StackID: "s1", Name: "Dev", Slug: "dev", Type: "static", Settings: "{}", Network: "n", FromKind: "branch", CreatedAt: t0}
	must(s.Environments.Create(ctx, e))
	must(s.Environments.SetPlacement(ctx, "e1", "p1"))
	e.Name = "Dev 2"
	must(s.Environments.Update(ctx, e))
	if got, _ := s.Environments.Get(ctx, "e1"); got.PlacementID != "p1" || got.Name != "Dev 2" {
		t.Errorf("after stale update = %+v", got)
	}
	if es, _ := s.Environments.ListByPlacement(ctx, "p1"); len(es) != 1 || es[0].ID != "e1" {
		t.Errorf("by placement = %+v", es)
	}
}
