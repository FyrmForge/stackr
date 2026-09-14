package graph

import "testing"

// The card is replica 1 and every replica above it is dealt underneath, capped
// so a tile at 30 replicas takes the same room as one at 4.
func TestApplyPlacementReplicas(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: AppNodeID("t1")}}}
	tasks := []Task{}
	for i := 1; i <= 6; i++ {
		state := "running"
		if i == 6 {
			state = "failed"
		}
		tasks = append(tasks, Task{Slot: i, State: state, NodeName: "n1"})
	}
	ApplyPlacement(&g, map[string]TileTasks{"t1": {Want: 6, Tasks: tasks}}, true)

	r := g.Nodes[0].Replicas
	if r.Want != 6 || r.Running != 5 {
		t.Fatalf("roll-up: %d/%d", r.Running, r.Want)
	}
	if !r.Degraded() {
		t.Fatal("one failed replica should read degraded")
	}
	if len(r.Rows) != replicaCap {
		t.Fatalf("dealt rows: %d, want %d", len(r.Rows), replicaCap)
	}
	if r.Rows[0].Slot != 2 {
		t.Fatalf("first dealt row is replica %d, want 2", r.Rows[0].Slot)
	}
	if r.Hidden != 2 || r.HiddenWord != "1 failed" {
		t.Fatalf("hidden: %d %q", r.Hidden, r.HiddenWord)
	}
}

// A single-node swarm is unchanged: no chip on any card.
func TestApplyPlacementSingleNode(t *testing.T) {
	g := Graph{Nodes: []Node{{ID: AppNodeID("t1")}}}
	ApplyPlacement(&g, map[string]TileTasks{
		"t1": {Want: 1, HomeNode: "n1", Tasks: []Task{{Slot: 1, State: "running", NodeName: "n1"}}},
	}, false)
	if g.Nodes[0].Node != "" || g.Nodes[0].Home {
		t.Fatal("a one-node swarm drew a node chip")
	}
}
