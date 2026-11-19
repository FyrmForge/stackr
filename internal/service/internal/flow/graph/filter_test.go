package graph

import "testing"

func TestFilterOperatedGhost(t *testing.T) {
	build := func() View {
		return View{
			Nodes: []Node{{ID: "slice", Kind: "slice"}, {ID: "pg", Kind: KindRef}},
			Edges: []Edge{{EdgeShared, "slice", "pg"}},
		}
	}
	off := All
	off.Operated = false
	v := build()
	filter(&v, off)
	if len(v.Nodes) != 1 || v.Nodes[0].ID != "slice" || len(v.Edges) != 0 {
		t.Fatalf("operated off: nodes %v edges %v", v.Nodes, v.Edges)
	}
	v = build()
	filter(&v, All)
	if len(v.Nodes) != 2 || len(v.Edges) != 1 {
		t.Fatalf("operated on: nodes %v edges %v", v.Nodes, v.Edges)
	}
}
