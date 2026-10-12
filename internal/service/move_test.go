package service

import (
	"context"
	"net/netip"
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

	if _, err := n.orch.MoveEnv(ctx, &Principal{}, n.dev.ID, "w1", false); err == nil {
		t.Fatal("a non-admin moved an env")
	}
	if _, err := n.orch.MoveEnv(ctx, admin, n.dev.ID, n.ctl.ID, false); err == nil {
		t.Fatal("moved an env to the node it is on")
	}
	j, err := n.orch.MoveEnv(ctx, admin, n.dev.ID, "w1", false)
	must(t, err)
	if _, err := n.orch.MoveEnv(ctx, admin, n.dev.ID, "w1", false); err == nil {
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

// The controller's row holds no wg address: a move to it takes the transit
// address its range implies.
func TestMoveAddrOfController(t *testing.T) {
	n := newTwoNodes(t)
	got, err := moveNodes{n.orch}.Addr(context.Background(), n.ctl.ID)
	must(t, err)
	r := netip.MustParsePrefix(n.ctl.RangeCIDR)
	if want, _ := mesh.TransitAddr(r); got != want.String() {
		t.Fatalf("controller addr = %q, want %s", got, want)
	}
}
