package metrics

import (
	"context"
	"testing"
	"time"
)

// Points go in, come back averaged per step; older than Keep are dropped.
func TestMemory(t *testing.T) {
	ctx := context.Background()
	t0 := time.Unix(1_000_000, 0)
	s := &Memory{Keep: 10 * time.Minute}
	var ps []Point
	for i := range 20 { // one a minute, 0..19
		ps = append(ps, Point{Key: "node:a", Name: "cpu_pct", T: t0.Add(time.Duration(i) * time.Minute), V: float64(i)})
	}
	if err := s.Write(ctx, ps); err != nil {
		t.Fatal(err)
	}
	got, err := s.Query(ctx, "node:a", t0, t0.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	xs := got["cpu_pct"]
	if len(xs) != 11 || xs[0].V != 9 || xs[10].V != 19 { // kept: minutes 9..19
		t.Fatalf("kept = %v", xs)
	}
	got, _ = s.Query(ctx, "node:a", t0.Add(10*time.Minute), t0.Add(19*time.Minute), 5*time.Minute)
	xs = got["cpu_pct"]
	if len(xs) != 2 || xs[0].V != 12 || xs[1].V != 17 { // 10..14 -> 12, 15..19 -> 17
		t.Fatalf("bucketed = %v", xs)
	}
	if got, _ := s.Query(ctx, "node:none", t0, t0.Add(time.Hour), 0); len(got) != 0 {
		t.Fatalf("unknown key = %v", got)
	}
}
