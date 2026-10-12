package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
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
	if _, err := n.orch.SetDefaultIngress(ctx, &Principal{}, "w1"); err == nil {
		t.Error("a non-admin moved the default ingress")
	}
	if _, err := n.orch.SetDefaultIngress(ctx, adm, n.ctl.ID+"x"); err == nil {
		t.Error("default ingress set to a missing node")
	}
	d0, err := n.orch.SetDefaultIngress(ctx, adm, "w1")
	must(t, err)
	if d0.Ingress != "w1" {
		t.Errorf("defaults = %+v", d0)
	}
	if id, _ := n.orch.DefaultIngress(ctx); id != "w1" {
		t.Errorf("default ingress = %s", id)
	}
	must(t, n.orch.SetNodeTags(ctx, adm, n.ctl.ID, []string{"builder"}))
	if _, err := n.orch.SetDefaultIngress(ctx, adm, n.ctl.ID); err == nil {
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

	// open to orgs
	d, err := n.orch.NodeDetail(ctx, "w1")
	must(t, err)
	if len(d.EnvList) != 1 || d.EnvList[0].Path != "acme / shop / edge" || len(d.Orgs) != 1 || d.Orgs[0].On {
		t.Fatalf("detail = %+v", d)
	}
	org := d.Orgs[0].ID
	if ok, _ := n.orch.NodeOpenToOrg(ctx, "w1", org); ok {
		t.Error("a worker is open before an admin opens it")
	}
	must(t, n.orch.SetNodeOrgs(ctx, adm, "w1", []string{org}))
	if ok, _ := n.orch.NodeOpenToOrg(ctx, "w1", org); !ok {
		t.Error("not open after opening")
	}
	if ok, _ := n.orch.NodeOpenToOrg(ctx, n.ctl.ID, org); !ok {
		t.Error("the controller is open to every org")
	}
	if err := n.orch.SetNodeOrgs(ctx, adm, n.ctl.ID, nil); err == nil {
		t.Error("controller org list was set")
	}

	// remove needs the node empty; the controller never goes
	if err := n.orch.RemoveNode(ctx, adm, "w1"); err == nil {
		t.Error("removed a node that runs an env")
	}
	if err := n.orch.RemoveNode(ctx, adm, n.ctl.ID); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("remove controller = %v", err)
	}

	// forget needs the name, then the env is lost
	if err := n.orch.ForgetNode(ctx, adm, "w1", "nope"); err == nil {
		t.Error("forgot without the name")
	}
	must(t, n.orch.ForgetNode(ctx, adm, "w1", "w1"))
	one(t, w, auditNodeForget)
	lost, err := n.orch.LostEnvs(ctx)
	must(t, err)
	if len(lost) != 1 || lost[0].Node != "w1" || lost[0].Path != "acme / shop / edge" {
		t.Fatalf("lost = %+v", lost)
	}
	if _, err := n.orch.RestoreLostEnv(ctx, adm, n.dev.ID, n.ctl.ID); err == nil {
		t.Error("restored an env that is not lost")
	}
	if _, err := n.orch.RestoreLostEnv(ctx, adm, lost[0].ID, n.ctl.ID); err != nil {
		t.Fatal(err)
	}
	if lost, _ = n.orch.LostEnvs(ctx); len(lost) != 0 {
		t.Errorf("still lost: %+v", lost)
	}
	if !slices.ContainsFunc(events(t, w, auditEnvRestore), func(e SecurityEvent) bool { return e.TargetID == n.edge.ID }) {
		t.Error("restore is not audited")
	}
}
