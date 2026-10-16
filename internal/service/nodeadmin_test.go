package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func TestNodeAdminLifecycle(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	adm := &Principal{User: User{ID: "u-admin"}, Access: authz.User{ID: "u-admin", Active: true, Admin: true}}
	w := &world{orch: n.orch, st: n.st}

	// not an admin: refused
	if err := n.orch.SetNodeTags(ctx, &Principal{}, "w1", []string{"builder"}); err == nil {
		t.Fatal("a non-admin set tags")
	}

	cards, err := n.orch.Nodes(ctx)
	must(t, err)
	if len(cards) != 2 || !cards[0].Controller || cards[1].Envs != 1 {
		t.Fatalf("cards = %+v", cards)
	}

	// the controller keeps ingress while no other node has it
	if err := n.orch.SetNodeTags(ctx, adm, n.ctl.ID, []string{"builder"}); err == nil {
		t.Error("controller lost ingress with no other ingress node")
	}
	must(t, n.orch.SetNodeTags(ctx, adm, "w1", []string{"ingress", "egress"}))
	// ...and keeps it while it is the default, even with another ingress node
	if err := n.orch.SetNodeTags(ctx, adm, n.ctl.ID, []string{"builder"}); err == nil {
		t.Error("controller lost ingress while it is the default")
	}
	if _, err := n.orch.SetDefaultIngress(ctx, &Principal{}, "w1", "w1"); err == nil {
		t.Error("a non-admin moved the default ingress")
	}
	if _, err := n.orch.SetDefaultIngress(ctx, adm, n.ctl.ID+"x", n.ctl.ID+"x"); err == nil {
		t.Error("default ingress set to a missing node")
	}
	if _, err := n.orch.SetDefaultIngress(ctx, adm, "w1", "nope"); err == nil {
		t.Error("default ingress moved without the node's name")
	}
	d0, err := n.orch.SetDefaultIngress(ctx, adm, "w1", "w1")
	must(t, err)
	if d0.Ingress != "w1" {
		t.Errorf("defaults = %+v", d0)
	}
	if id, _ := n.orch.DefaultIngress(ctx); id != "w1" {
		t.Errorf("default ingress = %s", id)
	}
	// ...and drops egress only once it has a default exit
	if err := n.orch.SetNodeTags(ctx, adm, n.ctl.ID, []string{"builder"}); !isConflict(err) {
		t.Errorf("controller dropped egress with no default exit: %v", err)
	}
	must(t, n.orch.SaveNodeTags(ctx, adm, n.ctl.ID, []string{"builder"}, []string{"w1"}))
	if _, err := n.orch.SetDefaultIngress(ctx, adm, n.ctl.ID, n.ctl.Name); err == nil {
		t.Error("default ingress set to a node without the tag")
	}
	if _, err := n.orch.SetDefaultExit(ctx, adm, n.ctl.ID); err == nil {
		t.Error("default exit set to a node without the tag")
	}
	if _, err := n.orch.SetDefaultExit(ctx, adm, "w1"); err != nil {
		t.Fatal(err)
	}
	if n.orch.DefaultExit(ctx) != "w1" || len(defaultExitIDs(ctx, n.st)) != 1 {
		t.Error("default exit not stored")
	}
	if err := n.orch.SetNodeTags(ctx, adm, "w1", []string{"ingress"}); err == nil {
		t.Error("w1 lost egress while it is the default exit")
	}
	if len(events(t, w, auditDefaultIngress)) != 1 || len(events(t, w, auditDefaultExit)) != 1 {
		t.Error("default changes are not audited")
	}
	if err := n.orch.SetNodeTags(ctx, adm, "w1", []string{"gpu"}); err == nil {
		t.Error("unknown tag accepted")
	}
	if len(events(t, w, auditNodeTags)) != 2 {
		t.Error("tag changes are not audited")
	}

	// the node's placements and envs
	n.placeW1(t, false)
	d, err := n.orch.NodeDetail(ctx, "w1")
	must(t, err)
	if len(d.EnvList) != 1 || d.EnvList[0].Path != "acme/shop/edge" || d.EnvList[0].Placement != "w1" || !slices.Equal(d.Placements, []string{"w1"}) {
		t.Fatalf("detail = %+v", d)
	}

	// remove needs the node empty and without placements; the controller never goes
	if _, err := n.orch.RemoveNode(ctx, adm, "w1"); err == nil {
		t.Error("removed a node that runs an env")
	}
	if _, err := n.orch.RemoveNode(ctx, adm, n.ctl.ID); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("remove controller = %v", err)
	}

	// forget needs the name, then the env is lost
	if _, err := n.orch.ForgetNode(ctx, adm, "w1", "nope"); err == nil {
		t.Error("forgot without the name")
	}
	// the controller leaves through w1 now: forgetting w1 would clear that silently
	if _, err := n.orch.ForgetNode(ctx, adm, "w1", "w1"); !isConflict(err) {
		t.Errorf("forgot the controller's default exit: %v", err)
	}
	must(t, n.orch.SetNodeTags(ctx, adm, n.ctl.ID, []string{"ingress", "egress"}))
	// one live volume and one orphan: only the live one is lost
	gone := time.Now()
	for _, v := range []store.Volume{{ID: "v-live", Slug: "data"}, {ID: "v-orphan", Slug: "old", OrphanedAt: &gone}} {
		v.ScopeKind, v.ScopeID, v.Name, v.CreatedAt = "env", n.edge.ID, v.ID, time.Now()
		must(t, n.st.Volumes.Create(ctx, v))
	}
	// w1 is down for good: no job, it is forgotten at once
	w1, _ := n.st.Nodes.Get(ctx, "w1")
	w1.Status = node.StatusOffline
	must(t, n.st.Nodes.Update(ctx, w1))
	if j, err := n.orch.ForgetNode(ctx, adm, "w1", "w1"); err != nil || j.ID != "" {
		t.Fatalf("forget an offline node = %+v, %v; want it gone at once", j, err)
	}
	one(t, w, auditNodeForget)
	lost, err := n.orch.LostEnvs(ctx)
	must(t, err)
	if len(lost) != 1 || lost[0].Node != "w1" || lost[0].Path != "acme/shop/edge" || lost[0].Volumes != 1 {
		t.Fatalf("lost = %+v", lost)
	}
	// the lost env names its placement: repointing that restores it
	if lost[0].Placement != "w1" {
		t.Errorf("lost env placement = %q, want w1", lost[0].Placement)
	}
}
