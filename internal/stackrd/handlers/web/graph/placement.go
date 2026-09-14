package graph

import (
	"fmt"
	"sort"
	"time"
)

// Placement decorates a built graph with where its cards actually run.
//
// Kept out of Build on purpose. Build is pure over the rows in the database;
// which node a task landed on and how many replicas answered is swarm's
// answer, read live, and folding it into Build would put a docker call behind
// every graph test (docs/plans/32-multi-node-ui.md, canvas).

// TileTasks is one tile's tasks as swarm reports them.
type TileTasks struct {
	// Want is the configured replica count.
	Want int
	// Tasks in slot order.
	Tasks []Task
	// HomeNode is set for a pinned tile: its volume is on that node's disk.
	HomeNode string
	// MovingTo and MovePct describe a volume move in flight.
	MovingTo string
	MovePct  int
}

// Task is one running replica.
type Task struct {
	Slot     int
	State    string
	NodeName string
	Started  time.Time
}

// replicaCap is how many replicas show in full: the card plus three dealt
// rows. Above that the fourth row is a "+n more" summary, so a tile at 3
// replicas and one at 30 take the same room (docs/plans/32-multi-node-ui.md).
const replicaCap = 3

// ApplyPlacement fills in the node chip, the replica roll-up and any move in
// flight. multiNode is false on a one-node swarm, and then no chip is drawn
// at all, a single-node install looks exactly as it did before any of this.
func ApplyPlacement(g *Graph, byTile map[string]TileTasks, multiNode bool) {
	for i := range g.Nodes {
		id := TileIDOf(g.Nodes[i].ID)
		if id == "" {
			continue
		}
		t, ok := byTile[id]
		if !ok {
			continue
		}
		apply(&g.Nodes[i], t, multiNode)
	}
}

func apply(n *Node, t TileTasks, multiNode bool) {
	tasks := append([]Task(nil), t.Tasks...)
	sort.Slice(tasks, func(a, b int) bool { return tasks[a].Slot < tasks[b].Slot })

	if multiNode {
		// The chip names replica 1's node; the rest carry their own.
		for _, task := range tasks {
			if task.Slot <= 1 {
				n.Node = task.NodeName
				break
			}
		}
		if n.Node == "" && len(tasks) > 0 {
			n.Node = tasks[0].NodeName
		}
		n.Home = t.HomeNode != ""
		n.MovingTo, n.MovePct = t.MovingTo, t.MovePct
	}

	if t.Want <= 1 {
		return
	}
	r := Replicas{Want: t.Want}
	for _, task := range tasks {
		if task.State == "running" {
			r.Running++
		}
		if task.Slot <= 1 {
			continue
		}
		if len(r.Rows) < replicaCap {
			r.Rows = append(r.Rows, ReplicaRow{
				Slot: task.Slot, State: task.State,
				Age: age(task.Started), Node: task.NodeName,
			})
			continue
		}
		r.Hidden++
	}
	if r.Hidden > 0 {
		r.HiddenWord = summarise(tasks[len(tasks)-r.Hidden:])
	}
	n.Replicas = r
}

// summarise is the one word the "+n more" row carries. The worst thing in the
// hidden set, because that is the only reason to open it.
func summarise(hidden []Task) string {
	failed, starting := 0, 0
	for _, t := range hidden {
		switch t.State {
		case "running":
		case "failed", "rejected", "orphaned":
			failed++
		default:
			starting++
		}
	}
	switch {
	case failed > 0:
		return fmt.Sprintf("%d failed", failed)
	case starting > 0:
		return fmt.Sprintf("%d starting", starting)
	}
	return "all running"
}

// TileIDOf pulls the tile id back out of a card id, for the card kinds that
// stand for a tile. "" for everything else (envs, connectors, the host).
func TileIDOf(nodeID string) string {
	// The card kinds that are a tile. Jobs are not: a job card stands for a
	// schedule, not for something with tasks of its own.
	for _, prefix := range []string{"app:", "db:"} {
		if len(nodeID) > len(prefix) && nodeID[:len(prefix)] == prefix {
			return nodeID[len(prefix):]
		}
	}
	return ""
}

func age(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
