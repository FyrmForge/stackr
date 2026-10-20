package service

import (
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/metrics"
)

// An open graph gets only its own series of the newest round, and only once.
func TestMetricsAfterScope(t *testing.T) {
	t0 := time.Unix(2_000_000, 0)
	tile := "b3d7aa59-c598-45e3"
	o := &Orchestrator{}
	o.metrics.seq, o.metrics.last = 7, []metrics.Point{
		{Key: "node:a", Name: "cpu_pct", T: t0, V: 1},
		{Key: "node:b", Name: "cpu_pct", T: t0, V: 2},
		{Key: LinkKey("b", "a"), Name: "rtt_ms", T: t0, V: 3},
		{Key: "tile:" + tile, Name: "mem", T: t0, V: 4},
		{Key: "ctr:" + tile + "/stackr-web-b3d7aa59-abc123-1", Name: "mem", T: t0, V: 5},
		{Key: "ctr:other/stackr-web-other-1", Name: "mem", T: t0, V: 6},
		{Key: "tile:other", Name: "mem", T: t0, V: 7},
	}
	body, seq := o.MetricsAfter(MetricsScope{key: "node:a"}, 6)
	if s, ok := body.(Series); !ok || seq != 7 || len(s) != 1 || s["cpu_pct"][0].V != 1 {
		t.Fatalf("node = %v %d", body, seq)
	}
	if body, _ := o.MetricsAfter(MetricsScope{key: "node:a"}, 7); body != nil {
		t.Fatalf("a seen round came again: %v", body)
	}
	if body, _ := o.MetricsAfter(MetricsScope{key: LinkKey("a", "b")}, 6); body.(Series)["rtt_ms"][0].V != 3 {
		t.Fatalf("link = %v", body)
	}
	body, _ = o.MetricsAfter(MetricsScope{key: "tile:" + tile, tile: tile}, 6)
	tm, ok := body.(TileMetricsOut)
	if !ok || len(tm.Tile) != 1 || tm.Tile["mem"][0].V != 4 || len(tm.Replicas) != 1 || tm.Replicas["abc123-1"]["mem"][0].V != 5 {
		t.Fatalf("tile = %+v", body)
	}
	if body, _ := o.MetricsAfter(MetricsScope{key: "node:zzz"}, 6); body != nil {
		t.Fatalf("empty scope sent %v", body)
	}
}
