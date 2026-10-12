package service

import (
	"context"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/authz"
)

func TestPlacement(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	admin := &Principal{User: User{ID: "u-admin"}, Access: authz.User{ID: "u-admin", Active: true, Admin: true}}
	org, err := n.orch.store.Stacks.Get(ctx, n.stack)
	must(t, err)

	// Only the controller is offered until a worker is opened to the org.
	ns, err := n.orch.OfferedNodes(ctx, org.OrgID)
	must(t, err)
	if len(ns) != 1 || !ns[0].Controller {
		t.Fatalf("offered before open = %+v", ns)
	}
	owner := &Principal{User: User{ID: "u-owner"}, Access: authz.User{ID: "u-owner", Active: true}}
	if err := n.orch.SetPin(ctx, owner, ScopeStack, n.stack, "w1"); err == nil {
		t.Fatal("an owner pinned to a node that is not open to the org")
	}
	if err := n.orch.SetNodeOrgs(ctx, &Principal{}, "w1", []string{org.OrgID}); err == nil {
		t.Fatal("a non-admin opened a node")
	}
	must(t, n.orch.SetNodeOrgs(ctx, admin, "w1", []string{org.OrgID}))

	// Pin the stack: the envs already there stay, and say so.
	must(t, n.orch.SetPin(ctx, admin, ScopeStack, n.stack, "w1"))
	v, err := n.orch.Placement(ctx, ScopeStack, n.stack)
	must(t, err)
	if v.Pin != "w1" || v.PinName != "w1" || v.Stay.Count != 1 || v.Stay.Where != n.ctl.Name {
		t.Errorf("stack placement = %+v", v)
	}
	ev, err := n.orch.Placement(ctx, ScopeEnv, n.edge.ID)
	must(t, err)
	if ev.Pin != "w1" || ev.PinFrom != "From stack: w1." {
		t.Errorf("env placement = %+v", ev)
	}

	// Ingress needs the tag; the nearest pick wins down the chain.
	if err := n.orch.SetIngress(ctx, admin, ScopeOrg, org.OrgID, "w1"); err == nil {
		t.Fatal("ingress on an untagged node")
	}
	w, err := n.st.Nodes.Get(ctx, "w1")
	must(t, err)
	w.Tags = `["ingress","egress"]`
	must(t, n.st.Nodes.Update(ctx, w))
	must(t, n.orch.SetIngress(ctx, admin, ScopeOrg, org.OrgID, "w1"))
	got, err := n.orch.IngressFor(ctx, n.edgeT.ID)
	must(t, err)
	ev, err = n.orch.Placement(ctx, ScopeEnv, n.edge.ID)
	must(t, err)
	if got != "w1" || ev.IngressName != "w1" || ev.IngressFrom != "From org: w1." || ev.Ingress != "" {
		t.Errorf("ingress for = %q, view = %+v", got, ev)
	}
	must(t, n.orch.SetIngress(ctx, admin, ScopeEnv, n.edge.ID, n.ctl.ID))
	if got, _ = n.orch.IngressFor(ctx, n.edgeT.ID); got != n.ctl.ID {
		t.Errorf("env pick lost to the org's: %q", got)
	}

	// Exit ladder: add, reorder, own rung, inherit, remove.
	must(t, n.orch.ExitsAdd(ctx, admin, ScopeStack, n.stack, "w1"))
	must(t, n.orch.ExitsAdd(ctx, admin, ScopeStack, n.stack, OwnRung))
	if err := n.orch.ExitsAdd(ctx, admin, ScopeStack, n.stack, "w1"); err == nil {
		t.Error("an exit listed twice")
	}
	if err := n.orch.ExitsAdd(ctx, admin, ScopeStack, n.stack, n.ctl.ID); err != nil {
		t.Errorf("controller is tagged egress: %v", err)
	}
	sv, err := n.orch.Placement(ctx, ScopeStack, n.stack)
	must(t, err)
	if len(sv.Rungs) != 3 || sv.Rungs[1].Name != "own node (no exit)" || !sv.OwnListed || sv.Rungs[0].Up != nil || !slices.Equal(sv.Rungs[0].Down, []string{OwnRung, "w1", n.ctl.ID}) {
		t.Fatalf("stack rungs = %+v", sv.Rungs)
	}
	ev, err = n.orch.Placement(ctx, ScopeEnv, n.edge.ID)
	must(t, err)
	if !ev.Inherited || len(ev.Rungs) != 3 {
		t.Errorf("env should show the stack's ladder: %+v", ev)
	}
	r, err := n.orch.ResolveExits(ctx, n.edgeT.ID)
	must(t, err)
	if r.Source != "stack" {
		t.Errorf("resolved from %q", r.Source)
	}
	must(t, n.orch.SetExits(ctx, admin, ScopeStack, n.stack, []string{n.ctl.ID, "w1"}))
	must(t, n.orch.ExitsRemove(ctx, admin, ScopeStack, n.stack, "w1"))
	must(t, n.orch.ExitsRemove(ctx, admin, ScopeStack, n.stack, n.ctl.ID))
	if sv, _ = n.orch.Placement(ctx, ScopeStack, n.stack); !sv.Inherited || len(sv.Rungs) != 0 {
		t.Errorf("emptied ladder should inherit: %+v", sv)
	}

	// Closing the node stops offering it.
	must(t, n.orch.SetNodeOrgs(ctx, admin, "w1", nil))
	if ns, _ = n.orch.OfferedNodes(ctx, org.OrgID); len(ns) != 1 {
		t.Errorf("offered after close = %+v", ns)
	}
}

func TestPlanMoveAndStatus(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	admin := &Principal{User: User{ID: "u-admin"}, Access: authz.User{ID: "u-admin", Active: true, Admin: true}}
	st, err := n.orch.store.Stacks.Get(ctx, n.stack)
	must(t, err)
	if _, err := n.orch.PlanMove(ctx, n.dev.ID, ""); err == nil {
		t.Fatal("planned a move with no node open to the org")
	}
	must(t, n.orch.SetNodeOrgs(ctx, admin, "w1", []string{st.OrgID}))
	p, err := n.orch.PlanMove(ctx, n.dev.ID, "w1")
	must(t, err)
	if p.From != n.ctl.Name || p.To != "w1" || len(p.Targets) != 1 {
		t.Errorf("plan = %+v", p)
	}
	if ms, err := n.orch.EnvMoveStatus(ctx, n.dev.ID); err != nil || ms != nil {
		t.Fatalf("status before any move = %+v, %v", ms, err)
	}
	if _, err := n.orch.MoveEnv(ctx, admin, n.dev.ID, "w1", true); err != nil {
		t.Fatal(err)
	}
	ms, err := n.orch.EnvMoveStatus(ctx, n.dev.ID)
	must(t, err)
	if ms == nil || ms.To != "w1" || len(ms.Steps) != 5 || ms.Steps[0].Label != "Copy while running" {
		t.Errorf("status = %+v", ms)
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 999: "999 B", 12_400_000_000: "12.4 GB", 1500: "1.5 kB"} {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
