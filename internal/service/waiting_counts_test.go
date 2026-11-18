package service

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
)

// The words match the review's summary: operated rows of a created tile fold
// into its create row; optional removals and deletes count as removals.
func TestPlanCounts(t *testing.T) {
	a, c, r := planCounts([]promote.Change{
		{Kind: "create", Tile: "pg"}, {Kind: "operated", Tile: "pg"}, {Kind: "update", Tile: "web"},
		{Kind: "param", Optional: true}, {Kind: "delete", Tile: "old"},
	})
	if a != 1 || c != 1 || r != 2 {
		t.Errorf("counts = %d, %d, %d", a, c, r)
	}
}
