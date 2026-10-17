package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The first boot after a restore deploys the controller's tiles that ran and
// lost their containers, once: a running one, a stopped one, a never
// deployed one and a worker's are left alone.
func TestRedeployAfterRestore(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	done := func(tl Tile, kind jobs.Kind, at time.Time) {
		t.Helper()
		must(t, w.st.Jobs.Create(ctx, store.Job{
			ID: uuid.NewString(), Kind: string(kind), State: job.Done, LockSet: store.StringList{tl.ID},
			Payload: "{}", CreatedAt: at,
		}))
	}
	old := time.Now().Add(-time.Hour)
	lost := w.tile(t, "lost", false)
	done(lost, kindDeploy, old)
	stopped := w.tile(t, "stopped", false)
	done(stopped, kindDeploy, old)
	done(stopped, kindStop, old.Add(time.Minute))
	never := w.tile(t, "never", false)
	up := w.tile(t, "up", true)
	done(up, kindDeploy, old)

	must(t, w.st.Nodes.Create(ctx, store.Node{
		ID: "w1", Name: "w1", Slug: "w1", Role: node.Worker, Tags: "[]", RangeCIDR: "10.230.0.0/16",
		Status: node.StatusOnline, CreatedAt: time.Now(),
	}))
	far := uuid.NewString()
	must(t, w.st.Environments.Create(ctx, store.Environment{
		NodeID: "w1", ID: far, StackID: w.stack, Name: "far", Slug: "far", Type: "static",
		Settings: "{}", Network: "f", FromKind: "branch", FromBranch: "main", CreatedAt: time.Now(),
	}))
	remote, err := w.orch.CreateTile(ctx, Tile{
		StackID: w.stack, EnvironmentID: far, Name: "remote", Kind: "image", ImageRef: "nginx:1", ContainerPort: 80,
	})
	must(t, err)
	done(remote, kindDeploy, old)

	marker := filepath.Join(w.orch.cfg.DataDir, installspec.RestoredMarker)
	must(t, os.WriteFile(marker, nil, 0o600))
	w.orch.redeployRestored(ctx)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker still there: %v", err)
	}
	deploys := func(tl Tile) int {
		js, err := w.orch.TileJobs(ctx, []string{tl.ID}, 10)
		must(t, err)
		n := 0
		for _, j := range js {
			if j.Kind == string(kindDeploy) && j.CreatedAt.After(old.Add(time.Minute)) {
				n++
			}
		}
		return n
	}
	if n := deploys(lost); n != 1 {
		t.Errorf("lost tile: %d new deploys, want 1", n)
	}
	for _, tl := range []Tile{stopped, never, up, remote} {
		if n := deploys(tl); n != 0 {
			t.Errorf("%s: %d new deploys, want 0", tl.Slug, n)
		}
	}
	w.orch.redeployRestored(ctx) // no marker: nothing more
	if n := deploys(lost); n != 1 {
		t.Errorf("second boot: %d deploys of the lost tile, want 1", n)
	}
}
