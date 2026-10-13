package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
	"github.com/FyrmForge/stackr/internal/worker"
)

// An offline node keeps the box it was last seen in and its remembered
// direct tunnels, drawn down; a pair it never had a tunnel with stays
// undrawn; a node cut off behind a down hop says so on its card.
func TestOfflineNodeKeepsBoxAndDownLinks(t *testing.T) {
	lan := worker.Facts{Interfaces: []worker.Iface{{Name: "enp7s0", Prefixes: []string{"10.0.0.3/16"}}}}
	in := []clusterNodeIn{
		{Node: store.Node{ID: "ctl", Name: "controller", Role: node.Controller, Status: node.StatusOnline}, Up: true},
		{Node: store.Node{ID: "h4", Name: "h4", Role: node.Worker, Status: node.StatusOffline},
			Mem: &nodeMemory{Net: "10.0.0.0/16", Links: map[string]string{"ctl": LinkInternet, "h3": LinkPrivate}}},
		{Node: store.Node{ID: "h3", Name: "h3", Role: node.Worker, Status: node.StatusOffline},
			Mem: &nodeMemory{Net: "10.0.0.0/16", Links: map[string]string{"h4": LinkPrivate}, Via: []string{"h4"}}},
		{Node: store.Node{ID: "w", Name: "w", Role: node.Worker, Status: node.StatusOnline}, Up: true, Facts: &lan},
	}
	m := buildClusterMap(in)
	for _, id := range []string{"h4", "h3"} {
		if n := m.Nodes[nodeIdx(m, id)]; n.Net != "10.0.0.0/16" {
			t.Errorf("%s net = %q, want its remembered box", id, n.Net)
		}
	}
	down := map[string]bool{}
	for _, l := range m.Links {
		if l.Down {
			down[l.A+" "+l.B] = true
		}
	}
	if !down["ctl h4"] || !down["h4 h3"] {
		t.Errorf("down links = %v, want ctl-h4 and h4-h3", down)
	}
	if down["ctl h3"] {
		t.Error("ctl-h3 was never a direct tunnel; drawn down")
	}
	routed := func(a, b string) (bool, bool) { return false, true }
	m.Links = directLinks(m.Links, m.Nodes, map[string]string{"h4": "h4"}, routed, func(a, b string) string { return "h4" })
	if len(m.Links) < 2 {
		t.Errorf("down links dropped as routed: %v", m.Links)
	}
	recall(&m, in)
	h3 := m.Nodes[nodeIdx(m, "h3")]
	if len(h3.ReachedVia) != 1 || h3.ReachedVia[0] != "h4" || len(h3.ViaDown) != 1 {
		t.Errorf("h3 via = %v down %v, want h4 (down)", h3.ReachedVia, h3.ViaDown)
	}
}

func nodeIdx(m ClusterMap, id string) int {
	for i, n := range m.Nodes {
		if n.ID == id {
			return i
		}
	}
	return -1
}

// Forgetting a node drops what the map remembered of it.
func TestRemoveNodeForgetsMemory(t *testing.T) {
	ctx := context.Background()
	st := storetest.Store(t)
	ctrl := storetest.Controller(t, st)
	m := newMeshSvc(st, func(string) (MeshNode, bool) { return nil, false })
	if err := st.Settings.Set(ctx, memoryKey("w1"), `{"net":"10.0.0.0/16"}`); err != nil {
		t.Fatal(err)
	}
	_ = ctrl
	if err := m.RemoveNode(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := st.Settings.Get(ctx, memoryKey("w1")); ok && v != "" {
		t.Errorf("memory kept after forget: %q", v)
	}
}
