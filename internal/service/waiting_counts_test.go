package service

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
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

// A plan's create rows name the env's node, and say a new volume lands on its
// disk once the node has a ready one.
func TestNodeAndDisk(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	if name, on := n.orch.nodeAndDisk(ctx, n.edge.ID); name != "w1" || on {
		t.Errorf("no disk: %q, %v", name, on)
	}
	must(t, n.st.NodeDisks.Create(ctx, store.NodeDisk{ID: "d1", NodeID: "w1", Kind: "path", MountPath: "/disk", State: disk.StateReady, CreatedAt: time.Now()}))
	if name, on := n.orch.nodeAndDisk(ctx, n.edge.ID); name != "w1" || !on {
		t.Errorf("ready disk: %q, %v", name, on)
	}
}
