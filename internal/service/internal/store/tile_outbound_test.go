package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Add sums bytes and moves last seen; ListByEnv holds only that env's tiles.
func TestTileOutbound(t *testing.T) {
	ctx := context.Background()
	env := servicetest.New(t)
	tl := env.Tile(t, env.Org(t, "acme"))
	other := env.Stack(t, env.Org(t, "beta"), "x")
	s := env.Store
	t1, t2 := t0, t0.Add(time.Minute)
	for _, a := range []struct {
		id string
		b  int64
		at time.Time
	}{{tl.ID, 100, t1}, {tl.ID, 50, t2}, {other.ID, 7, t1}} {
		if err := s.TileOutbound.Add(ctx, a.id, a.b, a.at); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.TileOutbound.ListByEnv(ctx, tl.Env)
	if err != nil || len(got) != 1 || got[0].TileID != tl.ID || got[0].Bytes != 150 || !got[0].LastSeen.Equal(t2) {
		t.Fatalf("ListByEnv = %+v, %v", got, err)
	}
}
