package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func planOf(t *testing.T, pl service.ServerPlan) serverconfig.Plan {
	t.Helper()
	var p serverconfig.Plan
	if err := json.Unmarshal([]byte(pl.Plan), &p); err != nil {
		t.Fatalf("plan json: %v\n%s", err, pl.Plan)
	}
	return p
}

func hasKind(p serverconfig.Plan, kind, tile string) bool {
	for _, c := range p.Changes {
		if c.Kind == kind && c.Tile == tile {
			return true
		}
	}
	return false
}

// settled waits for the plan to end applied and returns it.
func (r fileRig) settled(t *testing.T, id string) service.ServerPlan {
	t.Helper()
	var last service.ServerPlan
	eventually(t, "server plan "+id+" ended", func() bool {
		p, err := r.env.Orch.ServerPlan(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		last = p
		return p.Status == "applied" || p.Status == "error"
	})
	if last.Status != "applied" {
		t.Fatalf("server plan %s = %s: %s", id, last.Status, last.Error)
	}
	return last
}

func (r fileRig) apply(t *testing.T, file string) service.ServerPlan {
	t.Helper()
	ctx := context.Background()
	pl, err := r.env.Orch.PlanServerFile(ctx, []byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{Confirm: true}); err != nil {
		t.Fatal(err)
	}
	return r.settled(t, pl.ID)
}

// secondWorker is a second online worker; servicetest's Worker gives every
// node the same range.
func secondWorker(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	id := uuid.NewString()
	err := st.Nodes.Create(context.Background(), store.Node{
		ID: id, Name: name, Slug: name, Role: "worker", Tags: "[]", RangeCIDR: "10.210.0.0/16", WGAddress: "10.207.255.3",
		WGPublicKey: "wg-" + name, Status: "online", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedPlacement(t *testing.T, st *store.Store, name, nodeID string, open bool) string {
	t.Helper()
	id := uuid.NewString()
	err := st.Placements.Create(context.Background(), store.Placement{ID: id, Name: name, NodeID: nodeID, OpenToAll: open, CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Node tags in the file apply through SetNodeTags, a node the file leaves out
// is a note, and a declared node that has not joined is a gap with its tags.
func TestServerFileNodes(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	h3 := r.env.Worker(t, "h3", "ca")
	secondWorker(t, r.env.Store, "h4")

	pl, err := r.env.Orch.PlanServerFile(ctx, []byte("version: 1\nnodes:\n  h3: {tags: [storage, egress]}\n  h9: {tags: [ingress]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := planOf(t, pl)
	if !hasKind(p, "node-tags", "h3") {
		t.Errorf("changes = %+v, want node-tags h3", p.Changes)
	}
	if !strings.Contains(strings.Join(p.Notes, "|"), "h4 is not in the file (unmanaged)") {
		t.Errorf("notes = %q, want h4 unmanaged", p.Notes)
	}
	if len(p.Gaps) != 1 || p.Gaps[0].Kind != "node" || p.Gaps[0].Key != "h9" || len(p.Gaps[0].Tags) != 1 || p.Gaps[0].Tags[0] != "ingress" {
		t.Errorf("gaps = %+v, want Add this node for h9 with its tags", p.Gaps)
	}
	if p.Blocked() {
		t.Fatalf("blocked: %q", p.Blockers)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)
	n, err := r.env.Store.Nodes.Get(ctx, h3)
	if err != nil || !strings.Contains(n.Tags, "storage") || !strings.Contains(n.Tags, "egress") {
		t.Errorf("tags = %q, %v", n.Tags, err)
	}
	if _, err := r.env.Store.Nodes.GetBySlug(ctx, "h9"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("the file created node h9: %v", err)
	}
}

// An unset server secret is a gap the plan can fill, not a note.
func TestServerFileSecretGap(t *testing.T) {
	r := newFileRig(t)
	pl, err := r.env.Orch.PlanServerFile(context.Background(), []byte("version: 1\nparams:\n  s3:\n    key: {type: secret}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := planOf(t, pl)
	if len(p.Gaps) != 1 || p.Gaps[0].Kind != "secret" || p.Gaps[0].Scope != "server" || p.Gaps[0].Key != "s3.key" {
		t.Errorf("gaps = %+v", p.Gaps)
	}
}

// A placement on a node that has not joined waits, with the node as a gap.
func TestServerFilePlacementWaits(t *testing.T) {
	r := newFileRig(t)
	pl, err := r.env.Orch.PlanServerFile(context.Background(), []byte("version: 1\nnodes:\n  h4: {tags: [ingress]}\nplacements:\n  eu-prod: {node: h4}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := planOf(t, pl)
	if !hasKind(p, "placement", "eu-prod") || len(p.Gaps) != 1 || p.Gaps[0].Key != "h4" {
		t.Errorf("plan = %+v", p)
	}
	if !strings.Contains(strings.Join(p.Notes, "|"), "eu-prod waits for node h4") {
		t.Errorf("notes = %q", p.Notes)
	}
}

// New placements, a changed access list and a first node land through lane
// A's verbs; the default placement is never a removal row.
func TestServerFilePlacementsApply(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	r.env.Worker(t, "h3", "ca")
	r.env.Org(t, "acme")
	idle := seedPlacement(t, r.env.Store, "idle", "", true)

	r.apply(t, "version: 1\nplacements:\n  eu-db: {node: h3, open: [acme]}\n  idle: {node: h3}\n")

	db, err := r.env.Store.Placements.GetByName(ctx, "eu-db")
	if err != nil || db.OpenToAll || db.NodeID == "" {
		t.Errorf("eu-db = %+v, %v; want closed and on h3", db, err)
	}
	if got, err := r.env.Store.Placements.Get(ctx, idle); err != nil || got.NodeID != db.NodeID {
		t.Errorf("idle = %+v, %v; want a first node", got, err)
	}
	p, err := r.env.Orch.PlanServerFile(ctx, []byte("version: 1\nplacements:\n  eu-db: {node: h3, open: [acme]}\n  idle: {node: h3}\n"))
	if err != nil || p.Status != "clean" {
		t.Errorf("replan = %+v, %v; want clean", p, err)
	}
}

// A placement the file stops naming is a removal row, applied only ticked.
func TestServerFilePlacementRemoval(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	seedPlacement(t, r.env.Store, "old", "", true)

	pl, err := r.env.Orch.PlanServerFile(ctx, []byte("version: 1\nplacements:\n  default: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pl.Plan, `"key":"placement:old"`) || strings.Contains(pl.Plan, "placement:default") {
		t.Fatalf("plan = %s; want a removal row for old only", pl.Plan)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{Ticked: []string{"placement:old"}}); err != nil {
		t.Fatal(err)
	}
	r.settled(t, pl.ID)
	if _, err := r.env.Store.Placements.GetByName(ctx, "old"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("old is still there: %v", err)
	}
}

// A repoint the move checks pass is a placement-move change; one that moves
// envs is risky, so its approve needs the confirm.
func TestServerFileRepointPlan(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	h3 := r.env.Worker(t, "h3", "ca")
	secondWorker(t, r.env.Store, "h4")
	seedPlacement(t, r.env.Store, "eu-prod", h3, true)

	pl, err := r.env.Orch.PlanServerFile(ctx, []byte("version: 1\nplacements:\n  eu-prod: {node: h4}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p := planOf(t, pl); p.Blocked() || !hasKind(p, "placement-move", "eu-prod") {
		t.Fatalf("plan = %+v, want an unblocked placement-move", p)
	}

	// with an env on it the move is an impact line: no approve without the confirm
	tl := r.env.Tile(t, r.env.Org(t, "acme"))
	pid, _ := r.env.Store.Placements.GetByName(ctx, "eu-prod")
	if err := r.env.Store.Environments.SetPlacement(ctx, tl.Env, pid.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.env.Store.Environments.SetNode(ctx, tl.Env, h3); err != nil {
		t.Fatal(err)
	}
	if pl, err = r.env.Orch.PlanServerFile(ctx, []byte("version: 1\nplacements:\n  eu-prod: {node: h4}\n")); err != nil {
		t.Fatal(err)
	}
	p := planOf(t, pl)
	impact := ""
	for _, c := range p.Changes {
		if c.Kind == "placement-move" {
			impact = c.Impact
		}
	}
	if p.Blocked() || !strings.Contains(impact, "moves 1 env (acme/shop/dev) to h4") {
		t.Fatalf("plan = %+v, want the move as an impact line", p)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{}); !isConflict(err) {
		t.Errorf("approve without the confirm = %v, want a conflict", err)
	}
}

// Apply starts the move job and the plan ends applied while it runs.
func TestServerFileRepointApplies(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	h3 := r.env.Worker(t, "h3", "ca")
	h4 := secondWorker(t, r.env.Store, "h4")
	pid := seedPlacement(t, r.env.Store, "eu-prod", h3, true)

	pl, err := r.env.Orch.PlanServerFile(ctx, []byte("version: 1\nplacements:\n  eu-prod: {node: h4}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if planOf(t, pl).Blocked() {
		t.Fatalf("blocked: %q", planOf(t, pl).Blockers)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{Confirm: true}); err != nil {
		t.Fatal(err)
	}
	r.settled(t, pl.ID)
	p, err := r.env.Store.Placements.Get(ctx, pid)
	if err != nil || (p.NodeID != h4 && p.NodeID != h3) {
		t.Errorf("placement = %+v, %v", p, err)
	}
}

// The export of a box with nodes and placements parses and plans clean.
func TestServerExportNodesAndPlacements(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	h3 := r.env.Worker(t, "h3", "ca")
	r.env.Org(t, "acme")
	pid := seedPlacement(t, r.env.Store, "eu-db", h3, false)
	if err := r.env.Store.PlacementOrgs.Set(ctx, pid, []string{mustOrgID(t, r, "acme")}); err != nil {
		t.Fatal(err)
	}

	out, err := r.env.Orch.ExportServerConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"nodes:", "h3:", "placements:", "eu-db:", "node: h3", "- acme"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("export lacks %q:\n%s", want, out)
		}
	}
	pl, err := r.env.Orch.PlanServerFile(ctx, out)
	if err != nil || pl.Status != "clean" {
		t.Errorf("plan of the export = %+v, %v; want clean", pl, err)
	}
}

func mustOrgID(t *testing.T, r fileRig, slug string) string {
	t.Helper()
	og, err := r.env.Store.Orgs.GetBySlug(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return og.ID
}
