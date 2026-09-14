package project

import (
	"context"
	"log/slog"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/graph"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// markPlacement fills in each card's node chip, replica roll-up and any move
// in flight.
//
// Task state, not the local container list. A tile running happily on a worker
// has no container on the manager, and reading the manager's own daemon is
// what made such a tile show "stopped" (docs/plans/32-multi-node-ui.md,
// canvas, finding 2).
//
// Every failure here is silent by design: the canvas is the main screen, and
// a swarm call that times out must cost it a chip, not the page.
func (h *handler) markPlacement(ctx context.Context, g *graph.Graph, tiles []repo.Tile) {
	nodes, err := h.rt.ListNodes(ctx)
	if err != nil {
		slog.Debug("canvas: listing swarm nodes", "error", err)
		return
	}
	// One node is the common case and has nothing to say: no chips, and the
	// canvas is exactly what it was before any of this existed.
	multiNode := len(nodes) > 1
	nodeName := map[string]string{}
	for _, n := range nodes {
		nodeName[n.ID] = n.Hostname
	}

	byTile := map[string]graph.TileTasks{}
	for i := range tiles {
		t := tiles[i]
		if t.IsVolume() {
			continue // no service of its own; it is a mount on another tile
		}
		sc, err := envnet.Resolve(ctx, h.store, &t)
		if err != nil {
			continue
		}
		tt := graph.TileTasks{Want: max(t.Replicas, 1), HomeNode: nodeName[t.HomeNode]}
		if t.HomeNode != "" {
			tt.HomeNode = t.HomeNode
		}
		tasks, err := h.rt.ServiceTasksOnNetwork(ctx, sc.ServiceName(t.Slug), "")
		if err == nil {
			for _, task := range tasks {
				if task.DesiredState != "running" {
					continue
				}
				tt.Tasks = append(tt.Tasks, graph.Task{
					Slot: task.Slot, State: task.State, NodeName: nodeName[task.NodeID],
				})
			}
		}
		if h.mover != nil {
			if mv := h.mover.ForTile(t.ID); mv != nil {
				tt.MovingTo = nodeName[mv.To]
				if mv.Total > 0 {
					tt.MovePct = int(float64(mv.Bytes) / float64(mv.Total) * 100)
				}
			}
		}
		byTile[t.ID] = tt
	}
	graph.ApplyPlacement(g, byTile, multiNode)
}
