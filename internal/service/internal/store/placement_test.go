package store_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

func TestIngressPicks(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()
	if err := s.IngressPicks.Set(ctx, "env", "e1", "n1"); err != nil {
		t.Fatal(err)
	}
	if err := s.IngressPicks.Set(ctx, "env", "e1", "n2"); err != nil { // replaces
		t.Fatal(err)
	}
	if got, err := s.IngressPicks.Get(ctx, "env", "e1"); err != nil || got != "n2" {
		t.Fatalf("get = %q, %v", got, err)
	}
	if rows, _ := s.IngressPicks.ListByPlacement(ctx, "n2"); !reflect.DeepEqual(rows, []store.IngressPick{{ScopeKind: "env", ScopeID: "e1", PlacementID: "n2"}}) {
		t.Errorf("by placement = %+v", rows)
	}
	if err := s.IngressPicks.Set(ctx, "env", "e1", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.IngressPicks.Get(ctx, "env", "e1"); got != "" {
		t.Errorf("cleared pick = %q", got)
	}
}
