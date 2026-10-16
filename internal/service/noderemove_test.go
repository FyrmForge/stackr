package service

import (
	"context"
	"errors"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// fakeUninstall swaps the host helper: it records the node ids it ran for
// and runs check first (nil = nothing).
func fakeUninstall(t *testing.T, check func(id string)) *[]string {
	t.Helper()
	old := hostUninstall
	var ran []string
	hostUninstall = func(_ *Orchestrator, _ context.Context, id string, _ io.Writer) error {
		if check != nil {
			check(id)
		}
		ran = append(ran, id)
		return nil
	}
	t.Cleanup(func() { hostUninstall = old })
	return &ran
}

// nodeFake gives a seeded node its own Docker daemon behind the router.
func nodeFake(t *testing.T, n *twoNodes, id string) *dockerfake.Fake {
	t.Helper()
	f := dockerfake.New()
	n.orch.docker.(*DockerRouter).Set(id, f)
	return f
}

// removeNow queues the removal of a seeded node and waits for the job runner
// to finish it.
func removeNow(t *testing.T, n *twoNodes, id string) {
	t.Helper()
	if id != "w1" {
		nodeFake(t, n, id)
	}
	fakeUninstall(t, nil)
	j, err := n.orch.RemoveNode(context.Background(), adminP(), id)
	if err != nil {
		t.Fatal(err)
	}
	if j = n.wait(t, j); j.State != job.Done {
		t.Fatalf("remove %s: %s %s", id, j.State, j.Error)
	}
}

var managedLabel = map[string]string{docker.LabelManaged: "true"}

// seedRemovable is a worker with a running container, an orphan volume that
// has a row, a leftover volume that has none and a config-files volume.
func seedRemovable(t *testing.T, n *twoNodes, id string) (*dockerfake.Fake, store.Volume) {
	t.Helper()
	ctx := context.Background()
	seedNode(t, n, id, `[]`, "10.220.0.0/16")
	f := nodeFake(t, n, id)
	now := time.Now().UTC()
	orphan := store.Volume{
		ID: "vol-orphan", ScopeKind: "env", ScopeID: "gone-env", Slug: "pgdata", Name: "stackr-vol-orphan",
		CreatedAt: now, OrphanedAt: &now, NodeID: id,
	}
	must(t, n.st.Volumes.Create(ctx, orphan))
	f.Containers = []docker.Container{{ID: "c1", Name: "web", State: "running", Labels: managedLabel}}
	f.Volumes = []docker.VolumeInfo{
		{Name: orphan.Name, Labels: managedLabel, SizeBytes: 5},
		{Name: "stackr-vol-leftover", Labels: managedLabel, SizeBytes: 5},
		{Name: "stackr-files-x", Labels: map[string]string{docker.LabelManaged: "true", labelFilesTile: "t1"}},
	}
	return f, orphan
}

// Remove stops the box, backs up every volume into one group (an orphan as
// its row, a leftover by its Docker name, never the config files), checks
// it, runs the uninstall while the node is still known and only then drops
// the node and its orphan rows.
func TestRunRemoveNodeBacksUpThenUninstalls(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	f, orphan := seedRemovable(t, n, "w2")
	var rowThere bool
	ran := fakeUninstall(t, func(id string) {
		_, err := n.st.Nodes.Get(ctx, id)
		rowThere = err == nil
	})

	j, err := n.orch.RemoveNode(ctx, adminP(), "w2")
	must(t, err)
	if j.Kind != string(kindRemoveNode) {
		t.Errorf("job kind = %q", j.Kind)
	}
	if j = n.wait(t, j); j.State != job.Done { // the runner reaches runRemoveNode
		t.Fatalf("job %s: %s", j.State, j.Error)
	}

	if len(*ran) != 1 || (*ran)[0] != "w2" || !rowThere {
		t.Errorf("uninstall ran %v with the node row there = %v; want once, before the node is dropped", *ran, rowThere)
	}
	gs, err := n.st.BackupGroups.ListByNode(ctx, "w2")
	must(t, err)
	if len(gs) != 1 || gs[0].Reason != groupRemove || gs[0].Status != groupDone || !strings.HasSuffix(gs[0].Label, "before remove") {
		t.Fatalf("groups = %+v", gs)
	}
	runs, err := n.st.BackupRuns.ListByGroup(ctx, gs[0].ID)
	must(t, err)
	if len(runs) != 2 {
		t.Fatalf("runs = %+v, want the orphan and the leftover", runs)
	}
	// the orphan row goes with the node, which nulls its run's volume id; the
	// archive names are the Docker volumes
	seen := map[string]bool{}
	for _, r := range runs {
		if r.Status != "done" || r.SizeBytes <= 0 || r.GroupID == nil || *r.GroupID != gs[0].ID {
			t.Errorf("run = %+v", r)
		}
		for _, name := range []string{orphan.Name, "stackr-vol-leftover"} {
			if strings.Contains(r.ObjectKey, name) {
				seen[name] = true
			}
		}
	}
	if len(seen) != 2 {
		t.Errorf("archives of %v, want both volumes: %+v", seen, runs)
	}
	for _, name := range []string{orphan.Name, "stackr-vol-leftover"} {
		if !sawCall(f, "TarVolume("+name+")") {
			t.Errorf("%s was not archived: %v", name, callNames(f))
		}
	}
	if sawCall(f, "TarVolume(stackr-files-x)") {
		t.Error("a config-files volume was archived")
	}
	if !sawCall(f, "Stop(c1)") || sawCall(f, "Start(c1)") {
		t.Errorf("containers: stop once and stay stopped: %v", callNames(f))
	}
	if _, err := n.st.Nodes.Get(ctx, "w2"); err == nil {
		t.Error("the node row is still there")
	}
	if _, err := n.st.Volumes.Get(ctx, orphan.ID); err == nil {
		t.Error("the orphan row is still there")
	}
	views, err := n.orch.NodeBackups(ctx, "w2")
	must(t, err)
	if len(views) != 1 || len(views[0].Volumes) != 2 {
		t.Errorf("a removed node's group is still readable: %+v", views)
	}
}

// A tar that fails starts the containers again, fails the job naming the
// volume and leaves the node and the uninstall alone.
func TestRunRemoveNodeFailedBackupKeepsTheNode(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	f, _ := seedRemovable(t, n, "w2")
	f.Err = map[string]error{"TarVolume(stackr-vol-leftover)": errors.New("disk full")}
	ran := fakeUninstall(t, nil)

	err := n.orch.runRemoveNode(ctx, &jobs.Run{Log: io.Discard}, nodeJob{NodeID: "w2", By: "u-admin"})
	if err == nil || !strings.Contains(err.Error(), "stackr-vol-leftover") || !strings.Contains(err.Error(), "w2 not removed") {
		t.Fatalf("err = %v, want the volume and 'w2 not removed'", err)
	}
	if !sawCall(f, "Stop(c1)") || !sawCall(f, "Start(c1)") {
		t.Errorf("containers were not started again: %v", callNames(f))
	}
	if len(*ran) != 0 {
		t.Errorf("the box was cleaned after a failed backup: %v", *ran)
	}
	if _, err := n.st.Nodes.Get(ctx, "w2"); err != nil {
		t.Errorf("the node is gone: %v", err)
	}
	gs, _ := n.st.BackupGroups.ListByNode(ctx, "w2")
	if len(gs) != 1 || gs[0].Status != groupFailed {
		t.Errorf("groups = %+v, want one failed", gs)
	}
}

// The refusals: the default ingress, an offline node, and a node that is
// another node's default exit.
func TestRemoveNodeRefusals(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	seedNode(t, n, "h4", `["ingress"]`, "10.221.0.0/16")
	must(t, n.st.Settings.Set(ctx, settingDefaultIngress, "h4"))
	if _, err := n.orch.RemoveNode(ctx, adm, "h4"); !isConflict(err) || !strings.Contains(err.Error(), "default ingress") {
		t.Errorf("default ingress = %v, want node down", err)
	}

	seedNode(t, n, "h5", `[]`, "10.222.0.0/16")
	h5, err := n.st.Nodes.Get(ctx, "h5")
	must(t, err)
	h5.Status = node.StatusOffline
	must(t, n.st.Nodes.Update(ctx, h5))
	if _, err := n.orch.RemoveNode(ctx, adm, "h5"); !isNodeDown(err) {
		t.Errorf("offline = %v, want node down", err)
	}
	if _, err := n.orch.RemoveNode(ctx, nil, "h5"); err == nil {
		t.Error("a nil principal removed a node")
	}
}

// Forgetting a node that is another node's default exit is refused like a
// remove: it would silently clear that node's exit.
func TestForgetRefusesAnotherNodesDefaultExit(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	seedNode(t, n, "h4", `["egress"]`, "10.211.0.0/16")
	seedNode(t, n, "plain", `[]`, "10.212.0.0/16")
	if _, err := n.orch.SetNodeExit(ctx, adm, "plain", []string{"h4"}); err != nil {
		t.Fatal(err)
	}
	err := n.orch.ForgetNode(ctx, adm, "h4", "h4")
	if !isConflict(err) || !strings.Contains(err.Error(), "default exit of plain") {
		t.Fatalf("forget a default exit = %v, want a conflict naming plain", err)
	}
	if _, err := n.st.Nodes.Get(ctx, "h4"); err != nil {
		t.Fatalf("h4 is gone: %v", err)
	}
	if rungs, _ := n.st.ExitLadders.ListByTarget(ctx, "h4"); len(rungs) != 1 {
		t.Errorf("plain's exit was cleared: %v", rungs)
	}
	if _, err := n.orch.SetNodeExit(ctx, adm, "plain", []string{n.ctl.ID}); err != nil {
		t.Fatal(err)
	}
	must(t, n.orch.ForgetNode(ctx, adm, "h4", "h4"))
}

func isNodeDown(err error) bool {
	var d *NodeDownError
	return errors.As(err, &d)
}
