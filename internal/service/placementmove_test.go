package service

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

func pmConflict(err error) bool { _, ok := errs.IsConflict(err); return ok }
func pmInvalid(err error) bool  { _, ok := errs.IsInvalid(err); return ok }

type pmFix struct {
	*twoNodes
	admin *Principal
	eu    store.Placement // on the controller, holds dev
	m     *meshSvc
}

func pmAdmin() *Principal {
	return &Principal{User: User{ID: "u-admin"}, Access: authz.User{ID: "u-admin", Active: true, Admin: true}}
}

func (n *pmFix) placement(t *testing.T, name, nodeID string) store.Placement {
	t.Helper()
	p := store.Placement{ID: uuid.NewString(), Name: name, NodeID: nodeID, OpenToAll: true, CreatedAt: time.Now().UTC()}
	must(t, n.st.Placements.Create(context.Background(), p))
	return p
}

// newPMFix is two nodes, placement eu on the controller holding dev, w1 with
// the storage tag, and a mesh that knows both nodes' facts.
func newPMFix(t *testing.T) *pmFix {
	t.Helper()
	ctx := context.Background()
	n := &pmFix{twoNodes: newTwoNodes(t), admin: pmAdmin()}
	w1, err := n.st.Nodes.Get(ctx, "w1")
	must(t, err)
	w1.Tags, w1.WGAddress = `["storage"]`, mesh.ControllerTransit.Next().Next().String() // a live move reaches the node by its transit address
	must(t, n.st.Nodes.Update(ctx, w1))
	n.m = newMeshSvc(n.st, func(string) (MeshNode, bool) { return nil, false })
	n.m.facts[n.ctl.ID] = worker.Facts{Arch: "amd64", DiskFree: 500 << 30, DiskTotal: 1000 << 30, MemFree: 8 << 30}
	n.m.facts["w1"] = worker.Facts{Arch: "amd64", DiskFree: 500 << 30, DiskTotal: 1000 << 30, MemFree: 8 << 30}
	n.orch.mesh = n.m
	n.eu = n.placement(t, "eu", n.ctl.ID)
	must(t, n.st.Environments.SetPlacement(ctx, n.dev.ID, n.eu.ID))
	// what the target answers when dev lands on it
	n.ctlFake.RunID = "old"
	n.ctlFake.Details = map[string]docker.Detail{"old": {Running: true, Networks: map[string]string{n.dev.Network: "10.0.0.2"}}}
	n.wFake.RunID = "new"
	n.wFake.Details = map[string]docker.Detail{"new": {Running: true, Networks: map[string]string{n.dev.Network: "10.209.0.2"}}}
	return n
}

func failed(cs []planfile.Check) []string {
	var out []string
	for _, c := range cs {
		if !c.OK {
			out = append(out, c.Name)
		}
	}
	return out
}

func TestPlacementMoveChecksFailAlone(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, want string
		arrange    func(t *testing.T, n *pmFix)
	}{
		{"all pass", "", func(*testing.T, *pmFix) {}},
		{"target online", "target online", func(t *testing.T, n *pmFix) {
			w1, _ := n.st.Nodes.Get(ctx, "w1")
			w1.Status = node.StatusOffline
			must(t, n.st.Nodes.Update(ctx, w1))
		}},
		{"mesh link", "mesh link", func(_ *testing.T, n *pmFix) {
			n.m.up = map[[2]string]bool{mesh.PairKey(n.ctl.ID, "w1"): false}
		}},
		{"tags", "tags", func(t *testing.T, n *pmFix) {
			must(t, n.st.IngressPicks.Set(ctx, "env", n.dev.ID, n.eu.ID)) // eu is an ingress override
		}},
		{"disk", "disk", func(t *testing.T, n *pmFix) {
			must(t, n.st.Volumes.Create(ctx, store.Volume{ID: "v1", ScopeKind: "env", ScopeID: n.dev.ID, Slug: "data", Name: "data", CreatedAt: time.Now()}))
			n.ctlFake.Volumes = []docker.VolumeInfo{{Name: "data", SizeBytes: 5 << 30}}
			n.m.facts["w1"] = worker.Facts{Arch: "amd64", DiskFree: 1 << 30, DiskTotal: 100 << 30, MemFree: 8 << 30}
		}},
		{"memory", "memory", func(t *testing.T, n *pmFix) {
			tl := n.devT
			tl.MemLimitMB = 4096
			must(t, n.st.Tiles.Update(ctx, tl))
			n.m.facts["w1"] = worker.Facts{Arch: "amd64", DiskFree: 500 << 30, DiskTotal: 1000 << 30, MemFree: 1 << 30}
		}},
		{"cpu arch", "cpu arch", func(_ *testing.T, n *pmFix) {
			n.m.facts["w1"] = worker.Facts{Arch: "arm64", DiskFree: 500 << 30, DiskTotal: 1000 << 30, MemFree: 8 << 30}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := newPMFix(t)
			c.arrange(t, n)
			plan, err := n.orch.PlanPlacementMove(ctx, "eu", "w1")
			must(t, err)
			got := failed(plan.Checks)
			if c.want == "" && len(got) != 0 || c.want != "" && !slices.Equal(got, []string{c.want}) {
				t.Fatalf("failed = %v, want %q\n%+v", got, c.want, plan.Checks)
			}
			if plan.OK != (c.want == "") {
				t.Errorf("OK = %t", plan.OK)
			}
			if len(plan.Envs) != 1 || plan.From != n.ctl.Name || plan.To != "w1" || plan.Envs[0].Downtime != "about 10 s" {
				t.Errorf("plan = %+v", plan)
			}
		})
	}

	t.Run("source online", func(t *testing.T) {
		n := newPMFix(t)
		pw := n.placement(t, "edge-pl", "w1")
		must(t, n.st.Environments.SetPlacement(ctx, n.edge.ID, pw.ID))
		w1, _ := n.st.Nodes.Get(ctx, "w1")
		w1.Status = node.StatusOffline
		must(t, n.st.Nodes.Update(ctx, w1))
		plan, err := n.orch.PlanPlacementMove(ctx, "edge-pl", n.ctl.Name)
		must(t, err)
		if got := failed(plan.Checks); !slices.Equal(got, []string{"source online"}) {
			t.Fatalf("failed = %v\n%+v", got, plan.Checks)
		}
		if _, err := n.orch.MovePlacement(ctx, n.admin, "edge-pl", n.ctl.Name, "edge-pl"); !pmConflict(err) {
			t.Errorf("a blocked move = %v, want a conflict", err)
		}
	})
}

// A repoint moves its envs as jobs and the placement takes the new node when
// the last one lands.
func TestMovePlacementHappyPath(t *testing.T) {
	ctx := context.Background()
	n := newPMFix(t)
	if _, err := n.orch.MovePlacement(ctx, n.admin, "eu", "w1", "nope"); !pmInvalid(err) {
		t.Fatalf("wrong confirm = %v", err)
	}
	if _, err := n.orch.MovePlacement(ctx, nil, "eu", "w1", "eu"); err == nil {
		t.Fatal("moved with no principal")
	}
	j, err := n.orch.MovePlacement(ctx, n.admin, "eu", "w1", "eu")
	must(t, err)
	if j.ID == "" {
		t.Fatal("no job came back")
	}
	if j = n.wait(t, j); j.State != job.Done {
		t.Fatalf("move job = %s %q", j.State, j.Error)
	}
	e, err := n.orch.envs.Get(ctx, n.dev.ID)
	must(t, err)
	p, err := n.st.Placements.GetByName(ctx, "eu")
	must(t, err)
	if e.NodeID != "w1" || p.NodeID != "w1" {
		t.Fatalf("env on %q, placement on %q; want w1", e.NodeID, p.NodeID)
	}
	st, err := n.orch.PlacementMove(ctx, "eu")
	must(t, err)
	if st == nil || st.State != "done" || st.Done != 1 || st.All != 1 || len(st.Envs) != 1 || st.Envs[0].Path != "acme/shop/dev" || st.To != "w1" {
		t.Fatalf("status = %+v", st)
	}
	if ms, _ := n.st.Moves.ListByEnv(ctx, n.dev.ID); len(ms) != 1 || ms[0].PlacementMoveID == nil {
		t.Errorf("env moves = %+v", ms)
	}
	if _, err := n.orch.MovePlacement(ctx, n.admin, "eu", "w1", "eu"); !pmConflict(err) {
		t.Errorf("moving onto the node it is on = %v", err)
	}
	if evs := auditActions(t, n.orch, auditPlacementMove); len(evs) != 1 {
		t.Errorf("placement.move events = %+v", evs)
	}
}

func TestMovePlacementNoEnvsRepointsAtOnce(t *testing.T) {
	ctx := context.Background()
	n := newPMFix(t)
	n.placement(t, "empty", n.ctl.ID)
	j, err := n.orch.MovePlacement(ctx, n.admin, "empty", "w1", "empty")
	must(t, err)
	if j.ID != "" {
		t.Errorf("job = %+v, want none", j)
	}
	p, _ := n.st.Placements.GetByName(ctx, "empty")
	if p.NodeID != "w1" {
		t.Errorf("placement on %q", p.NodeID)
	}
	if st, _ := n.orch.PlacementMove(ctx, "empty"); st == nil || st.State != "done" {
		t.Errorf("status = %+v", st)
	}
}

// One env failing leaves the placement on the old node; Retry finishes it.
func TestMovePlacementFailureAndRetry(t *testing.T) {
	ctx := context.Background()
	n := newPMFix(t)
	n.wFake.Err = map[string]error{"Run": context.DeadlineExceeded}
	j, err := n.orch.MovePlacement(ctx, n.admin, "eu", "w1", "eu")
	must(t, err)
	if j = n.wait(t, j); j.State != job.Failed {
		t.Fatalf("move job = %s", j.State)
	}
	p, _ := n.st.Placements.GetByName(ctx, "eu")
	e, _ := n.orch.envs.Get(ctx, n.dev.ID)
	if p.NodeID != n.ctl.ID || e.NodeID != n.ctl.ID {
		t.Fatalf("placement on %q, env on %q; want the controller", p.NodeID, e.NodeID)
	}
	st, err := n.orch.PlacementMove(ctx, "eu")
	must(t, err)
	if st.State != "failed" || st.Error == "" || st.Done != 0 {
		t.Fatalf("status = %+v", st)
	}
	if _, err := n.orch.RetryPlacementMove(ctx, n.admin, "nope"); err == nil {
		t.Error("retried a placement that does not exist")
	}

	n.wFake.Err = nil
	j, err = n.orch.RetryPlacementMove(ctx, n.admin, "eu")
	must(t, err)
	if j = n.wait(t, j); j.State != job.Done {
		t.Fatalf("retry job = %s %q", j.State, j.Error)
	}
	p, _ = n.st.Placements.GetByName(ctx, "eu")
	if p.NodeID != "w1" {
		t.Errorf("placement on %q after retry", p.NodeID)
	}
	if st, _ = n.orch.PlacementMove(ctx, "eu"); st.State != "done" || st.Done != 1 {
		t.Errorf("status = %+v", st)
	}
	if _, err := n.orch.RetryPlacementMove(ctx, n.admin, "eu"); !pmConflict(err) {
		t.Errorf("retry of a finished move = %v", err)
	}
}

// D6: a placement with no node gets one, and its waiting env deploys there.
func TestPlacementNodeSetDeploysWaitingEnvs(t *testing.T) {
	ctx := context.Background()
	n := newPMFix(t)
	wait := n.placement(t, "wait", "")
	must(t, n.st.Environments.SetNode(ctx, n.dev.ID, ""))
	must(t, n.st.Environments.SetPlacement(ctx, n.dev.ID, wait.ID))

	plan, err := n.orch.PlanPlacementMove(ctx, "wait", "w1")
	must(t, err)
	if !plan.OK || len(plan.Envs) != 1 || !strings.Contains(plan.Envs[0].Downtime, "waiting") || plan.From != "" {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := n.orch.MovePlacement(ctx, n.admin, "wait", "w1", "wait"); err != nil {
		t.Fatal(err)
	}
	p, _ := n.st.Placements.GetByName(ctx, "wait")
	e, _ := n.orch.envs.Get(ctx, n.dev.ID)
	if p.NodeID != "w1" || e.NodeID != "w1" {
		t.Fatalf("placement on %q, env on %q; want w1", p.NodeID, e.NodeID)
	}
	js, err := n.st.Jobs.ListTouching(ctx, []string{n.devT.ID}, string(kindDeploy), 10)
	must(t, err)
	if len(js) != 1 {
		t.Fatalf("deploy jobs = %d, want 1", len(js))
	}
	n.wait(t, js[0])
	if err := n.orch.placementNodeSet(ctx, n.admin, wait.ID, "w1"); err == nil {
		t.Error("gave a node to a placement that has one")
	}
}

// D8: a repoint off a forgotten node rehomes the env, deploys it and queues
// the restore of its newest backup.
func TestMovePlacementRestoresAfterForget(t *testing.T) {
	ctx := context.Background()
	n := newPMFix(t)
	must(t, n.st.ForgottenNodes.Create(ctx, store.ForgottenNode{ID: "gone", Name: "h3", ForgottenAt: time.Now()}))
	lost := n.placement(t, "lost", "gone")
	must(t, n.st.Environments.SetNode(ctx, n.edge.ID, "gone"))
	must(t, n.st.Environments.SetPlacement(ctx, n.edge.ID, lost.ID))
	must(t, n.st.Volumes.Create(ctx, store.Volume{ID: "v-lost", ScopeKind: "env", ScopeID: n.edge.ID, Slug: "data", Name: "data", CreatedAt: time.Now()}))
	vid := "v-lost"
	must(t, n.st.BackupDests.Create(ctx, store.BackupDest{ID: "d", Kind: "s3", Name: "d", Endpoint: "e", Bucket: "b", AccessKey: "a", SecretKey: "s", CreatedAt: time.Now()}))
	must(t, n.st.BackupRuns.Create(ctx, store.BackupRun{ID: "r1", Kind: "volume", VolumeID: &vid, DestID: "d", Status: backup.Done, ObjectKey: "k.tar.age", CreatedAt: time.Now()}))

	plan, err := n.orch.PlanPlacementMove(ctx, "lost", "w1")
	must(t, err)
	if !plan.Restore || !plan.OK || plan.From != "h3" || len(plan.Envs) != 1 || plan.Envs[0].Bytes != -1 {
		t.Fatalf("plan = %+v", plan)
	}
	j, err := n.orch.MovePlacement(ctx, n.admin, "lost", "w1", "lost")
	must(t, err)
	if j.ID == "" {
		t.Fatal("no deploy job")
	}
	e, _ := n.orch.envs.Get(ctx, n.edge.ID)
	p, _ := n.st.Placements.GetByName(ctx, "lost")
	if e.NodeID != "w1" || p.NodeID != "w1" {
		t.Fatalf("env on %q, placement on %q; want w1", e.NodeID, p.NodeID)
	}
	pm, err := n.st.PlacementMoves.Latest(ctx, lost.ID)
	must(t, err)
	if pm.State != "done" || !pm.Restore || pm.Error != "" {
		t.Errorf("move row = %+v", pm)
	}
	rs, err := n.st.Jobs.ListByState(ctx, "queued", "running", "done", "failed", "waiting")
	must(t, err)
	// the restore comes first and holds the tile, so its deploy waits for it
	slices.Reverse(rs) // oldest first
	ri := slices.IndexFunc(rs, func(j store.Job) bool { return j.Kind == string(kindRestore) })
	di := slices.IndexFunc(rs, func(j store.Job) bool {
		return j.Kind == string(kindDeploy) && slices.Contains(j.LockSet, n.edgeT.ID)
	})
	switch {
	case ri < 0 || di < 0:
		t.Fatalf("restore at %d, deploy at %d among %d jobs", ri, di, len(rs))
	case ri > di:
		t.Errorf("the deploy was queued before the restore")
	case !slices.Contains(rs[ri].LockSet, n.edgeT.ID):
		t.Errorf("restore locks %v, not the tile %s", rs[ri].LockSet, n.edgeT.ID)
	}
}

// D4: a pin change that moves an env needs the plan confirmed; one that does
// not just records the placement. The plan never names a node.
func TestEnvPinMove(t *testing.T) {
	ctx := context.Background()
	n := newPMFix(t)
	pw := n.placement(t, "pw", "w1")

	same, err := n.orch.PlanEnvPin(ctx, n.dev.ID, "eu")
	must(t, err)
	if len(same.Envs) != 0 || !same.OK {
		t.Fatalf("same-node plan = %+v", same)
	}
	plan, err := n.orch.PlanEnvPin(ctx, n.dev.ID, "pw")
	must(t, err)
	if len(plan.Envs) != 1 || !plan.OK || plan.From != "eu" || plan.To != "pw" || plan.Placement != "acme/shop/dev" {
		t.Fatalf("plan = %+v", plan)
	}
	for _, c := range plan.Checks {
		if strings.Contains(c.Detail, "w1") || strings.Contains(c.Detail, n.ctl.Name) {
			t.Errorf("check %q names a node: %q", c.Name, c.Detail)
		}
	}
	if (&MoveNeeded{Plan: plan}).Error() == "" {
		t.Error("empty MoveNeeded message")
	}
	if _, err := n.orch.MoveEnvToPlacement(ctx, n.admin, n.dev.ID, "pw", "nope"); !pmInvalid(err) {
		t.Fatalf("wrong confirm = %v", err)
	}
	j, err := n.orch.MoveEnvToPlacement(ctx, n.admin, n.dev.ID, "pw", "dev")
	must(t, err)
	if j = n.wait(t, j); j.State != job.Done {
		t.Fatalf("move job = %s %q", j.State, j.Error)
	}
	e, _ := n.orch.envs.Get(ctx, n.dev.ID)
	if e.NodeID != "w1" || e.PlacementID != pw.ID {
		t.Fatalf("env on %q in %q; want w1 in pw", e.NodeID, e.PlacementID)
	}

	// no node change: recorded without a confirm
	other := n.placement(t, "pw2", "w1")
	if j, err := n.orch.MoveEnvToPlacement(ctx, n.admin, n.dev.ID, "pw2", ""); err != nil || j.ID != "" {
		t.Fatalf("same-node pin = %+v, %v", j, err)
	}
	if e, _ = n.orch.envs.Get(ctx, n.dev.ID); e.PlacementID != other.ID {
		t.Errorf("placement = %q", e.PlacementID)
	}
	if _, err := n.orch.PlanEnvPin(ctx, n.dev.ID, "nowhere"); err == nil {
		t.Error("planned a pin to a placement that does not exist")
	}
}

// moveChecks is the contract's entry point: node-named checks for a list of envs.
func TestMoveChecksNamesNodes(t *testing.T) {
	n := newPMFix(t)
	cs, err := n.orch.moveChecks(context.Background(), []string{n.dev.ID}, n.ctl.ID, "w1")
	must(t, err)
	if len(cs) != 7 || len(failed(cs)) != 0 || cs[0].Name != "target online" || !strings.Contains(cs[0].Detail, "w1") {
		t.Fatalf("checks = %+v", cs)
	}
}

// An org file pin that moves an env: the approved apply moves it, acting as
// the file (no principal of its own), and the org ends pinned.
func TestFilePinMovesEnv(t *testing.T) {
	ctx := context.Background()
	n := newPMFix(t)
	pw := n.placement(t, "pw", "w1")
	st, err := n.st.Stacks.Get(ctx, n.stack)
	must(t, err)
	must(t, n.orch.setPin(ctx, st, ScopeOrg, st.OrgID, "pw", []string{envPath(st, n.dev)}))
	js, err := n.st.Jobs.ListTouching(ctx, []string{n.devT.ID}, string(kindMoveEnv), 10)
	must(t, err)
	if len(js) != 1 {
		t.Fatalf("move jobs = %d, want 1", len(js))
	}
	if j := n.wait(t, js[0]); j.State != job.Done {
		t.Fatalf("move job = %s %q", j.State, j.Error)
	}
	e, _ := n.orch.envs.Get(ctx, n.dev.ID)
	og, _ := n.st.Orgs.Get(ctx, st.OrgID)
	if e.NodeID != "w1" || e.PlacementID != pw.ID || og.PlacementID == nil || *og.PlacementID != pw.ID {
		t.Fatalf("env on %q in %q, org pin %v; want w1 in pw, pinned", e.NodeID, e.PlacementID, og.PlacementID)
	}
}
