package service

import (
	"context"
	"errors"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// edgeData puts a managed postgres (with its instance volume) and a plain
// volume in the env on w1, both tiles running there.
func edgeData(t *testing.T, n *twoNodes) (pg Tile, inst, plain store.Volume) {
	t.Helper()
	ctx := context.Background()
	pg, err := n.orch.CreateManagedTile(ctx, Tile{EnvironmentID: n.edge.ID, Name: "pg"}, "postgres")
	must(t, err)
	m, err := n.st.ManagedInstances.GetByTile(ctx, pg.ID)
	must(t, err)
	inst, _, err = n.orch.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: n.edge.ID}, "pg-data", 0, &m.ID)
	must(t, err)
	plain, _, err = n.orch.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: n.edge.ID}, "uploads", 0, nil)
	must(t, err)
	for _, tl := range []Tile{pg, n.edgeT} {
		n.wFake.Containers = append(n.wFake.Containers, docker.Container{
			ID: "c-" + tl.Slug, Name: tl.Slug, State: "running",
			Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"},
		})
	}
	n.wFake.ExecOut = "SELECT 1;\n"
	return pg, inst, plain
}

func indexOf(f *dockerfake.Fake, prefix string) int {
	return slices.IndexFunc(f.Calls(), func(c dockerfake.Call) bool { return strings.HasPrefix(c.String(), prefix) })
}

func runNodeBackupNow(n *twoNodes) error {
	return n.orch.runNodeBackup(context.Background(), &jobs.Run{Log: io.Discard}, nodeJob{NodeID: "w1", By: "u-admin"})
}

// The engine volume is dumped while its env runs; then the tiles stop once,
// the plain volume is tarred and the tiles start once. Both runs sit in one
// group, which carries the local-fallback warning while no destination is set.
func TestRunNodeBackupDumpsThenStopsTarsStarts(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	pg, inst, plain := edgeData(t, n)

	j, err := n.orch.BackupNode(ctx, adminP(), "w1")
	must(t, err)
	if j.Kind != string(kindNodeBackup) || !slices.Contains([]string(j.LockSet), "node:w1") ||
		!slices.Contains([]string(j.LockSet), pg.ID) || !slices.Contains([]string(j.LockSet), n.edgeT.ID) {
		t.Errorf("job = %s locks %v; want the node and its tiles", j.Kind, j.LockSet)
	}
	if _, err := n.orch.BackupNode(ctx, &Principal{}, "w1"); err == nil {
		t.Error("a non-admin backed a node up")
	}
	if j = n.wait(t, j); j.State != job.Done { // the runner reaches runNodeBackup
		t.Fatalf("job %s: %s", j.State, j.Error)
	}

	dump, stop, tar, start := indexOf(n.wFake, "ExecStream("), indexOf(n.wFake, "Stop("), indexOf(n.wFake, "TarVolume("), indexOf(n.wFake, "Start(")
	if dump < 0 || stop < 0 || tar < 0 || start < 0 || dump >= stop || stop >= tar || tar >= start {
		t.Errorf("order dump %d stop %d tar %d start %d: %v", dump, stop, tar, start, callNames(n.wFake))
	}
	for _, c := range []string{"c-pg", "c-" + n.edgeT.Slug} {
		var stops, starts int
		for _, call := range callNames(n.wFake) {
			switch call {
			case "Stop(" + c + ")":
				stops++
			case "Start(" + c + ")":
				starts++
			}
		}
		if stops != 1 || starts != 1 {
			t.Errorf("%s stopped %d and started %d times, want once each", c, stops, starts)
		}
	}
	if sawCall(n.wFake, "TarVolume("+inst.Name+")") {
		t.Error("the engine volume was tarred as well as dumped")
	}
	if !sawCall(n.wFake, "TarVolume("+plain.Name+")") {
		t.Errorf("the plain volume was not tarred: %v", callNames(n.wFake))
	}

	gs, err := n.st.BackupGroups.ListByNode(ctx, "w1")
	must(t, err)
	if len(gs) != 1 || gs[0].Reason != groupManual || gs[0].Status != groupDone || gs[0].Warning == "" || !strings.HasPrefix(gs[0].Label, "node w1, ") {
		t.Fatalf("groups = %+v", gs)
	}
	runs, err := n.st.BackupRuns.ListByGroup(ctx, gs[0].ID)
	must(t, err)
	if len(runs) != 2 {
		t.Fatalf("runs = %+v, want two", runs)
	}
	views, err := n.orch.NodeBackups(ctx, "w1")
	must(t, err)
	if len(views) != 1 || views[0].Warning == "" || views[0].Dest != "local" || len(views[0].Volumes) != 2 {
		t.Fatalf("view = %+v", views)
	}
	methods := map[string]string{}
	for _, v := range views[0].Volumes {
		methods[v.Volume] = v.Method
		if v.Status != "done" || v.SizeBytes <= 0 || v.Where != "acme/shop/edge" {
			t.Errorf("volume row = %+v", v)
		}
	}
	if methods["pg-data"] != "dump" || methods["uploads"] != "tar" {
		t.Errorf("methods = %v", methods)
	}
}

// A global S3 destination is used and silences the warning.
func TestNodeBackupUsesAGlobalDestination(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	_, _, _ = edgeData(t, n)
	d, err := n.orch.CreateBackupDest(ctx, nil, BackupDestSpec{Name: "off", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	must(t, err)
	got, warn, err := n.orch.groupDest(ctx)
	must(t, err)
	if got.ID != d.ID || warn != "" || got.ArchiveKey == "" {
		t.Errorf("dest = %s (%q) key set %v, want the global one with its key and no warning", got.Name, warn, got.ArchiveKey != "")
	}
}

// A tar that fails still starts the tiles again, and the group says failed.
func TestRunNodeBackupFailedTarStartsTheTiles(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	_, _, plain := edgeData(t, n)
	n.wFake.Err = map[string]error{"TarVolume(" + plain.Name + ")": errors.New("disk full")}

	err := runNodeBackupNow(n)
	if err == nil || !strings.Contains(err.Error(), "uploads") {
		t.Fatalf("err = %v, want the failed volume named", err)
	}
	if !sawCall(n.wFake, "Start(c-pg)") || !sawCall(n.wFake, "Start(c-"+n.edgeT.Slug+")") {
		t.Errorf("tiles were not started again: %v", callNames(n.wFake))
	}
	gs, _ := n.st.BackupGroups.ListByNode(ctx, "w1")
	if len(gs) != 1 || gs[0].Status != groupFailed || !strings.Contains(gs[0].Error, "uploads") {
		t.Errorf("groups = %+v", gs)
	}
}

// BackupEveryNode queues one job per online node.
func TestBackupEveryNodeQueuesOnePerOnlineNode(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	seedNode(t, n, "h4", `[]`, "10.230.0.0/16")
	off, err := n.st.Nodes.Get(ctx, "h4")
	must(t, err)
	off.Status = "offline"
	must(t, n.st.Nodes.Update(ctx, off))
	js, err := n.orch.BackupEveryNode(ctx, adminP())
	must(t, err)
	if len(js) != 2 { // the controller and w1
		t.Errorf("jobs = %d, want 2", len(js))
	}
	if _, err := n.orch.BackupNode(ctx, adminP(), "h4"); !isNodeDown(err) {
		t.Errorf("offline node = %v, want node down", err)
	}
}
