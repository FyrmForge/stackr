package service

import (
	"context"
	"slices"
	"testing"
)

// An env on a node without egress leaves through that node's default exit:
// the exit lists it under ExitOthers and the map names the pair. An env that
// leaves from its own node is that node's ExitOwn.
func TestClusterMapEgressSplit(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	adm := nodeExitAdmin()
	seedNode(t, n, "h5", `["egress"]`, "10.211.0.0/16")
	_, err := n.orch.SetNodeExit(ctx, adm, "w1", []string{"h5"})
	must(t, err)

	m, err := n.orch.ClusterMap(ctx)
	must(t, err)
	by := map[string]ClusterNode{}
	for _, c := range m.Nodes {
		by[c.ID] = c
	}
	if len(by["h5"].ExitOthers) != 1 || len(by["h5"].ExitOwn) != 0 {
		t.Errorf("h5 own %v others %v, want only the other node's env", by["h5"].ExitOwn, by["h5"].ExitOthers)
	}
	if len(by["w1"].ExitOwn) != 0 || len(by["w1"].ExitOthers) != 0 {
		t.Errorf("w1 own %v others %v, want none: its traffic leaves at h5", by["w1"].ExitOwn, by["w1"].ExitOthers)
	}
	if len(by[n.ctl.ID].ExitOwn) == 0 || len(by[n.ctl.ID].ExitOthers) != 0 {
		t.Errorf("controller own %v others %v, want its own envs only", by[n.ctl.ID].ExitOwn, by[n.ctl.ID].ExitOthers)
	}
	if len(m.Egress) != 1 || m.Egress[0].From != "w1" || m.Egress[0].To != "h5" || !slices.Equal(m.Egress[0].Envs, by["h5"].ExitOthers) {
		t.Errorf("egress = %+v, want w1 to h5", m.Egress)
	}

	// No exit left for the node: its envs have no internet and no arrow.
	must(t, n.st.ExitLadders.Set(ctx, scopeNode, "w1", nil))
	m, err = n.orch.ClusterMap(ctx)
	must(t, err)
	if len(m.Egress) != 0 {
		t.Errorf("egress = %+v, want none for a node with nowhere to go", m.Egress)
	}
	for _, c := range m.Nodes {
		if c.ID != n.ctl.ID && (len(c.ExitOwn) != 0 || len(c.ExitOthers) != 0) {
			t.Errorf("%s has exits %v %v", c.Name, c.ExitOwn, c.ExitOthers)
		}
	}
}
