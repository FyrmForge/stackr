package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Upsert inserts, then rewrites spec, enabled and share and keeps created_at.
func TestPGRecipeUpsert(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()
	if err := s.PGRecipes.Upsert(ctx, store.PGRecipe{ID: "r", Spec: `{"a":1}`, Enabled: true, CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.PGRecipes.Upsert(ctx, store.PGRecipe{ID: "r", Spec: `{"a":2}`, Share: store.NameList{"o1"}, CreatedAt: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.PGRecipes.Get(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec != `{"a":2}` || got.Enabled || len(got.Share) != 1 || !got.CreatedAt.Equal(t0) {
		t.Errorf("after second upsert: %+v", got)
	}
	if rows, err := s.PGRecipes.List(ctx); err != nil || len(rows) != 1 {
		t.Errorf("List = %d rows, %v", len(rows), err)
	}
	if err := s.PGRecipes.Delete(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PGRecipes.Get(ctx, "r"); err == nil {
		t.Error("deleted recipe still readable")
	}
	if rows, err := s.PGRecipes.List(ctx); err != nil || len(rows) != 0 {
		t.Errorf("List = %d rows, %v", len(rows), err)
	}
}
