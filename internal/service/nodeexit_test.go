package service

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

func nodeExitAdmin() *Principal {
	return &Principal{User: User{ID: "u-admin"}, Access: authz.User{ID: "u-admin", Active: true, Admin: true}}
}

// addNode makes a confirmed worker with the given tags JSON.
func seedNode(t *testing.T, n *twoNodes, id, tags, cidr string) {
	t.Helper()
	must(t, n.st.Nodes.Create(context.Background(), store.Node{
		ID: id, Name: id, Slug: id, Role: node.Worker, Tags: tags, RangeCIDR: cidr,
		Status: node.StatusOnline, CreatedAt: time.Now(),
	}))
}

// envExits: an egress node leaves by its own uplink unless a ladder says
// otherwise; a node without egress never lists itself and falls through env,
// stack, org, its own default exit, the cluster default, then to no internet.
func TestEnvExits(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	adm := nodeExitAdmin()
	seedNode(t, n, "h4", `["egress"]`, "10.210.0.0/16")
	h4p, w1p := addPlacement(t, n, "h4", "h4", true), addPlacement(t, n, "w1", "w1", true)
	seedNode(t, n, "h5", `["egress"]`, "10.211.0.0/16")

	dev, err := n.st.Environments.Get(ctx, n.dev.ID)
	must(t, err)
	edge, err := n.st.Environments.Get(ctx, n.edge.ID)
	must(t, err)
	stk, err := n.st.Stacks.Get(ctx, n.stack)
	must(t, err)
	ctl, w1 := n.ctl, mustNode(t, n, "w1")
	if hasEgress(w1) || !hasEgress(ctl) {
		t.Fatalf("fixture: w1 egress %v, controller egress %v", hasEgress(w1), hasEgress(ctl))
	}
	got := func(e store.Environment, nd store.Node) ([]string, bool) {
		t.Helper()
		l, own, err := envExits(ctx, n.st, e, stk.OrgID, nd)
		must(t, err)
		return l, own
	}
	want := func(name string, l []string, own bool, wl []string, wown bool) {
		t.Helper()
		if !slices.Equal(l, wl) || own != wown {
			t.Errorf("%s: ladder %v own %v, want %v own %v", name, l, own, wl, wown)
		}
	}

	l, own := got(dev, ctl)
	want("egress node, nothing set", l, own, nil, true)

	// Nothing for a node without egress: no internet, and it is not own.
	l, own = got(edge, w1)
	want("no egress, nothing set", l, own, nil, false)

	_, err = n.orch.SetDefaultExit(ctx, adm, "h4")
	must(t, err)
	l, own = got(edge, w1)
	want("no egress, cluster default", l, own, []string{"h4"}, false)
	l, own = got(dev, ctl)
	want("egress node, cluster default", l, own, []string{"h4"}, false)

	// The node's own default exit beats the cluster default.
	_, err = n.orch.SetNodeExit(ctx, adm, "w1", []string{"h5"})
	must(t, err)
	l, own = got(edge, w1)
	want("no egress, node default", l, own, []string{"h5"}, false)
	// ...and applies only to a node without egress.
	l, _ = got(dev, ctl)
	if slices.Contains(l, "h5") {
		t.Errorf("a node ladder reached an egress node: %v", l)
	}

	// An env ladder naming the own node is stripped on a node without egress.
	must(t, n.orch.SetExits(ctx, adm, ScopeEnv, n.edge.ID, []string{OwnRung}))
	l, own = got(edge, w1)
	want("no egress, env ladder own", l, own, nil, false)
	// ...and is the own uplink on one with.
	must(t, n.orch.SetExits(ctx, adm, ScopeEnv, n.dev.ID, []string{OwnRung}))
	l, own = got(dev, ctl)
	want("egress node, env ladder own", l, own, nil, true)

	// The org's ladder beats the node's default.
	must(t, n.st.ExitLadders.Set(ctx, "env", n.edge.ID, nil))
	must(t, n.orch.SetExits(ctx, adm, ScopeOrg, stk.OrgID, []string{"h4"}))
	l, own = got(edge, w1)
	want("org beats node default", l, own, []string{"h4"}, false)

	// A ladder whose rungs all lack egress is no internet, not the own uplink.
	must(t, n.st.ExitLadders.Set(ctx, "env", n.dev.ID, []string{w1p.ID}))
	l, own = got(dev, ctl)
	want("egress node, ladder of non-egress rungs", l, own, nil, false)
	// Itself plus another exit keeps both, so the own uplink is a rung.
	must(t, n.st.ExitLadders.Set(ctx, "env", n.dev.ID, []string{mustPlacement(t, n, DefaultPlacement).ID, h4p.ID}))
	l, own = got(dev, ctl)
	want("egress node, self then h4", l, own, []string{n.ctl.ID, "h4"}, false)
}

// gather hands the mesh every env of a node without egress (an empty ladder
// is a blackhole, and the node fences) and none of an egress node that leaves
// by its own uplink.
func TestGatherEgressModel(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	n.orch.mesh = newMeshSvc(n.st, func(string) (MeshNode, bool) { return nil, false })
	_, err := n.st.DB().ExecContext(ctx, `UPDATE environments SET subnet = '10.208.9.0/24' WHERE id = ?`, n.edge.ID)
	must(t, err)
	find := func(in meshIn, id string) meshNodeIn {
		t.Helper()
		for _, x := range in.Nodes {
			if x.ID == id {
				return x
			}
		}
		t.Fatalf("node %s not gathered", id)
		return meshNodeIn{}
	}
	in, err := n.orch.mesh.gather(ctx)
	must(t, err)
	if c := find(in, n.ctl.ID); c.NoUplink || len(c.Envs) != 0 {
		t.Errorf("egress node with nothing set: %+v", c)
	}
	w := find(in, "w1")
	if !w.NoUplink || len(w.Envs) != 1 || len(w.Envs[0].Ladder) != 0 {
		t.Errorf("node without egress and no exit: %+v", w)
	}

	seedNode(t, n, "h4", `["egress"]`, "10.210.0.0/16")
	_, err = n.orch.SetNodeExit(ctx, nodeExitAdmin(), "w1", []string{"h4"})
	must(t, err)
	in, err = n.orch.mesh.gather(ctx)
	must(t, err)
	if w = find(in, "w1"); !w.NoUplink || len(w.Envs) != 1 || !slices.Equal(w.Envs[0].Ladder, []string{"h4"}) {
		t.Errorf("node with a default exit: %+v", w)
	}
}

func mustNode(t *testing.T, n *twoNodes, id string) store.Node {
	t.Helper()
	nd, err := n.st.Nodes.Get(context.Background(), id)
	must(t, err)
	return nd
}

func TestSetNodeExitRules(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	adm := nodeExitAdmin()
	seedNode(t, n, "h4", `["egress"]`, "10.210.0.0/16")
	seedNode(t, n, "h5", `["egress"]`, "10.211.0.0/16")
	seedNode(t, n, "h6", `["egress"]`, "10.212.0.0/16")
	seedNode(t, n, "plain", `["builder"]`, "10.213.0.0/16")

	if _, err := n.orch.SetNodeExit(ctx, &Principal{}, "w1", []string{"h4"}); err == nil {
		t.Error("a non-admin set an exit")
	}
	for name, refs := range map[string][]string{
		"itself":      {"w1"},
		"a duplicate": {"h4", "h4"},
		"no egress":   {"plain"},
		"four":        {"h4", "h5", "h6", n.ctl.ID},
		"missing":     {"nope"},
	} {
		if _, err := n.orch.SetNodeExit(ctx, adm, "w1", refs); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
	// A node without egress cannot clear its exits.
	_, err := n.orch.SetNodeExit(ctx, adm, "w1", nil)
	if !isConflict(err) {
		t.Errorf("clear without egress = %v, want conflict", err)
	}

	_, err = n.orch.SetNodeExit(ctx, adm, "w1", []string{"h4", "h5"})
	must(t, err)
	v, err := n.orch.NodeExits(ctx, "w1")
	must(t, err)
	if !slices.Equal(v.Ladder, []string{"h4", "h5"}) {
		t.Errorf("ladder = %v", v.Ladder)
	}
	var opts []string
	for _, o := range v.Options {
		opts = append(opts, o.Name)
	}
	slices.Sort(opts)
	if !slices.Equal(opts, slices.Sorted(slices.Values([]string{"h4", "h5", "h6", n.ctl.Name}))) {
		t.Errorf("options = %v", opts)
	}
	if len(events(t, &world{orch: n.orch, st: n.st}, auditNodeExit)) != 1 {
		t.Error("the exit change is not audited")
	}

	// Suggested is the exit w1 measured the lowest ping to.
	m := n.orch.mesh
	if m == nil {
		t.Skip("no mesh in this fixture")
	}
	for i, id := range []string{"h4", "h5", "h6"} {
		nd := mustNode(t, n, id)
		nd.WGAddress = "10.207.255." + strconv.Itoa(10+i)
		must(t, n.st.Nodes.Update(ctx, nd))
	}
	tr := func(id string) string { return transitOf(mustNode(t, n, id)).String() }
	m.mu.Lock()
	m.facts["w1"] = worker.Facts{Ping: map[string]worker.PeerPing{tr("h4"): {RTTms: 30}, tr("h5"): {RTTms: 12}, tr("h6"): {}}}
	m.mu.Unlock()
	if v, _ = n.orch.NodeExits(ctx, "w1"); v.Suggested != "h5" {
		t.Errorf("suggested = %q, want h5", v.Suggested)
	}

	// Gaining egress drops the node's default exit.
	must(t, n.orch.SetNodeTags(ctx, adm, "w1", []string{"egress"}))
	if v, _ = n.orch.NodeExits(ctx, "w1"); len(v.Ladder) != 0 {
		t.Errorf("ladder after gaining egress = %v", v.Ladder)
	}
}

// Dropping egress needs a default exit first, and a node that is somebody's
// default exit cannot be removed.
func TestEgressNeedsDefaultExit(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	adm := nodeExitAdmin()
	seedNode(t, n, "h4", `["egress"]`, "10.210.0.0/16")
	seedNode(t, n, "plain", `["builder"]`, "10.213.0.0/16")

	// untag egress without exits: conflict
	err := n.orch.SetNodeTags(ctx, adm, "h4", []string{"builder"})
	if !isConflict(err) {
		t.Fatalf("untag without exits = %v, want conflict", err)
	}
	if !slices.Contains(node.Tags(mustNode(t, n, "h4")), node.TagEgress) {
		t.Error("the refused untag still changed the tags")
	}
	// the controller is no different
	if err := n.orch.SetNodeTags(ctx, adm, n.ctl.ID, []string{"ingress", "builder"}); !isConflict(err) {
		t.Errorf("controller untag without exits = %v, want conflict", err)
	}
	// untag with exits: ok, through the one-call form
	if err := n.orch.SaveNodeTags(ctx, adm, "h4", []string{"builder"}, []string{n.ctl.ID}); err != nil {
		t.Fatalf("untag with exits: %v", err)
	}
	if hasEgress(mustNode(t, n, "h4")) {
		t.Error("egress still there")
	}
	if v, _ := n.orch.NodeExits(ctx, "h4"); !slices.Equal(v.Ladder, []string{n.ctl.Name}) {
		t.Errorf("ladder = %v", v.Ladder)
	}
	// an exit that is not tagged egress is refused in the same call
	if err := n.orch.SaveNodeTags(ctx, adm, "w1", []string{"builder"}, []string{"plain"}); err == nil {
		t.Error("exit without egress accepted")
	}

	// the controller is h4's default exit now: it cannot be removed (it never
	// can), but a worker that is someone's exit is refused by name
	seedNode(t, n, "h5", `["egress"]`, "10.211.0.0/16")
	if _, err := n.orch.SetNodeExit(ctx, adm, "plain", []string{"h5"}); err != nil {
		t.Fatal(err)
	}
	_, err = n.orch.RemoveNode(ctx, adm, "h5")
	if !isConflict(err) {
		t.Fatalf("remove a default exit = %v, want conflict", err)
	}
	if _, err := n.orch.SetNodeExit(ctx, adm, "plain", []string{n.ctl.ID}); err != nil {
		t.Fatal(err)
	}
	removeNow(t, n, "h5")
}
