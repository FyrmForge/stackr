package service

import (
	"bytes"
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
)

// Deleting a worker's env keeps its volume as an orphan pinned to that node;
// the orphan sweep reaches the node, takes the last backup there and removes
// the Docker volume there, not on the controller.
func TestDeleteEnvOrphansVolumeOnItsNode(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	e, err := n.orch.CreateEnv(ctx, n.stack, "tmp", EnvSpec{
		NodeID: "w1", Type: environment.Static, FromKind: environment.FromBranch, FromBranch: "main",
	})
	must(t, err)
	v, _, err := n.orch.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: e.ID}, "data", nil)
	must(t, err)
	n.wFake.Volumes = []docker.VolumeInfo{{Name: v.Name, SizeBytes: 10}}

	must(t, n.orch.DeleteEnv(ctx, e.ID))

	got, err := n.orch.volumes.Get(ctx, v.ID)
	must(t, err)
	if got.OrphanedAt == nil || got.NodeID != "w1" {
		t.Fatalf("volume = orphaned %v on %q, want orphaned on w1", got.OrphanedAt, got.NodeID)
	}

	local, err := n.orch.localDest(ctx)
	must(t, err)
	var log bytes.Buffer
	must(t, n.orch.backup.Orphans(ctx, 0, local, &log))

	if !sawCall(n.wFake, "TarVolume("+v.Name+")") || !sawCall(n.wFake, "RemoveVolume("+v.Name+")") {
		t.Errorf("worker: backup then remove expected: %v", callNames(n.wFake))
	}
	if sawCall(n.ctlFake, "TarVolume("+v.Name+")") || sawCall(n.ctlFake, "RemoveVolume("+v.Name+")") {
		t.Errorf("controller touched the worker's orphan: %v", callNames(n.ctlFake))
	}
	runs, err := n.st.BackupRuns.ListByKind(ctx, "volume")
	must(t, err)
	if len(runs) != 1 || runs[0].Trigger != "orphan" || runs[0].Status != "done" {
		t.Errorf("runs = %+v, want one finished orphan backup", runs)
	}
	if _, err := n.orch.volumes.Get(ctx, v.ID); err == nil {
		t.Error("the orphan row is still there")
	}
}
