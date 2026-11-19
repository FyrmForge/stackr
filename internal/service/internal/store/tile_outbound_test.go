package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
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
	rows := []store.TileOutbound{{TileID: tl.ID, Bytes: 100, LastSeen: t1}, {TileID: tl.ID, Bytes: 50, LastSeen: t2}, {TileID: other.ID, Bytes: 7, LastSeen: t1},
		{TileID: "gone", Bytes: 1, LastSeen: t1}} // a deleted tile is skipped, not an error
	if err := s.TileOutbound.AddAll(ctx, rows); err != nil {
		t.Fatal(err)
	}
	got, err := s.TileOutbound.ListByEnv(ctx, tl.Env)
	if err != nil || len(got) != 1 || got[0].TileID != tl.ID || got[0].Bytes != 150 || !got[0].LastSeen.Equal(t2) {
		t.Fatalf("ListByEnv = %+v, %v", got, err)
	}
}
