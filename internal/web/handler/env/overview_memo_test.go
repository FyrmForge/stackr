package env

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Viewers of one tile share a single overview read per LiveEvery; a gone
// viewer starts none.
func TestOverviewSharedAcrossViewers(t *testing.T) {
	e := servicetest.New(t)
	tile := e.Tile(t, e.Org(t, "acme"))
	h := NewHandler(e.Orch)
	s := service.Scope{Tile: &service.Tile{ID: tile.ID}}
	old := LiveEvery
	LiveEvery = time.Hour
	t.Cleanup(func() { LiveEvery = old })

	read := func(ctx context.Context) (uintptr, error) {
		o, err := h.overviewOf(ctx, s, nil, false, "/acme/shop/dev")
		return reflect.ValueOf(o.Unread).Pointer(), err
	}
	a, err := read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := read(context.Background()); a != b {
		t.Error("a second viewer read the overview again inside LiveEvery")
	}
	LiveEvery = 0
	if c, _ := read(context.Background()); a == c {
		t.Error("the overview was not read again after LiveEvery")
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := read(gone); err == nil {
		t.Error("a viewer that left started a read")
	}
}
