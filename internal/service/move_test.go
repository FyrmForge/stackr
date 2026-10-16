package service

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/move"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// A cold move: the env stops on the controller, starts on the worker in the
// worker's range, the old containers go, and the security log has both ends.
func TestMoveEnvColdToWorker(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	admin := &Principal{User: User{ID: "u-admin"}, Access: authz.User{ID: "u-admin", Active: true, Admin: true}}

	n.ctlFake.Containers = append(n.ctlFake.Containers, docker.Container{
		ID: "old", Name: "old", State: "running", Image: "nginx:1",
		Labels: map[string]string{tile.LabelTile: n.devT.ID, tile.LabelRole: "replica"},
	})
	n.ctlFake.Details = map[string]docker.Detail{"old": {Running: true}}
	n.wFake.RunID = "new"
	n.wFake.Details = map[string]docker.Detail{"new": {Running: true, Networks: map[string]string{n.dev.Network: "10.209.0.2"}}}

	if _, err := n.startMove(admin, n.dev.ID, n.ctl.ID, false); err == nil {
		t.Fatal("moved an env to the node it is on")
	}
	j, err := n.startMove(admin, n.dev.ID, "w1", false)
	must(t, err)
	if _, err := n.startMove(admin, n.dev.ID, "w1", false); err == nil {
		t.Error("a second move while one runs was accepted")
	}
	if j = n.wait(t, j); j.State != job.Done {
		t.Fatalf("move job = %s %q\nw: %v\nctl: %v", j.State, j.Error, callNames(n.wFake), callNames(n.ctlFake))
	}

	e, err := n.orch.envs.Get(ctx, n.dev.ID)
	must(t, err)
	if e.NodeID != "w1" {
		t.Errorf("env on %q, want w1", e.NodeID)
	}
	if !sawCall(n.ctlFake, "Stop(old)") || !sawCall(n.ctlFake, "StopRemove(old)") {
		t.Errorf("source: stop then remove expected: %v", callNames(n.ctlFake))
	}
	if !sawCall(n.wFake, "EnsureNetwork("+n.dev.Network+")") || !sawCall(n.wFake, "Run(") {
		t.Errorf("target did not start the env: %v", callNames(n.wFake))
	}
	m, err := n.st.Moves.ListByEnv(ctx, n.dev.ID)
	must(t, err)
	if len(m) != 1 || m[0].Phase != move.Done {
		t.Fatalf("moves = %+v", m)
	}
	for _, a := range []string{securitylog.MoveStart, securitylog.MoveFinish} {
		evs, err := n.orch.SecurityLog(ctx, SecurityFilter{Action: a})
		if err != nil || len(evs) != 1 || evs[0].TargetID != n.dev.ID || evs[0].ActorID != "u-admin" {
			t.Errorf("%s events = %+v, %v", a, evs, err)
		}
	}
}

// startMove is the move core every move path ends in: begin, then queue.
func (n *twoNodes) startMove(by *Principal, envID, to string, live bool) (Job, error) {
	ctx := context.Background()
	m, err := n.orch.moveFlow().Begin(ctx, move.Request{EnvID: envID, From: n.ctl.ID, To: to, Live: live})
	if err != nil {
		return Job{}, err
	}
	return n.orch.enqueueMove(ctx, m, by.User.ID, by.KeyID)
}

// A restart resumes a move that was cut off: one job for the unfinished move.
func TestResumeMovesQueuesUnfinished(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	m, err := n.orch.moveFlow().Begin(ctx, move.Request{EnvID: n.dev.ID, From: n.ctl.ID, To: "w1"})
	must(t, err)
	must(t, n.orch.resumeMoves(ctx))
	js, err := n.st.Jobs.ListTouching(ctx, []string{n.devT.ID}, string(kindMoveEnv), 10)
	must(t, err)
	var got int
	for _, j := range js {
		if strings.Contains(j.Payload, m.ID) {
			got++
		}
	}
	if got != 1 {
		t.Errorf("move jobs for %s = %d, want 1", m.ID, got)
	}
}

func TestDeployOrderPutsDependenciesFirst(t *testing.T) {
	ts := []Tile{
		{Slug: "web", Kind: tile.Image, DependsOn: "api"},
		{Slug: "api", Kind: tile.Image, DependsOn: "db:healthy"},
		{Slug: "db", Kind: tile.Image},
		{Slug: "pg", Kind: tile.Managed},
	}
	var got []string
	for _, x := range deployOrder(ts) {
		got = append(got, x.Slug)
	}
	if strings.Join(got, " ") != "pg db api web" {
		t.Errorf("order = %v", got)
	}
}

// A controller row from before transit addresses were stored holds none: a
// move to it takes the controller's fixed transit address.
func TestMoveAddrOfController(t *testing.T) {
	n := newTwoNodes(t)
	ctl := n.ctl
	ctl.WGAddress = ""
	must(t, n.st.Nodes.Update(context.Background(), ctl))
	got, err := moveNodes{n.orch}.Addr(context.Background(), n.ctl.ID)
	must(t, err)
	if got != mesh.ControllerTransit.String() {
		t.Fatalf("controller addr = %q, want %s", got, mesh.ControllerTransit)
	}
}
