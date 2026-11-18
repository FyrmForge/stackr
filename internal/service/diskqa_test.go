package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The node-wide disk jobs lock every tile on the node, but they are not the
// tile's last job.
func TestDiskJobsAreNotATilesLastJob(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	d.addDisk(t)
	j, err := d.orch.CheckNodeDisk(ctx, adminP(), "w1")
	d.run(t, j, err)
	if got, ok, err := d.orch.jobRows.LastShown(ctx, d.edgeT.ID); err != nil || (ok && strings.HasPrefix(got.Kind, "disk-")) {
		t.Errorf("shown last job of a tile = %s %v, %v", got.Kind, ok, err)
	}
	// liveness checks still see it
	if got, ok, err := d.orch.jobRows.Last(ctx, d.edgeT.ID); err != nil || !ok || got.Kind != "disk-check" {
		t.Errorf("Last = %s %v, %v; want the disk-check job", got.Kind, ok, err)
	}
	if st, err := d.orch.TileStatus(ctx, d.edgeT.ID); err != nil || (st.LastJob != nil && strings.HasPrefix(st.LastJob.Kind, "disk-")) {
		t.Errorf("tile status shows %+v, %v", st.LastJob, err)
	}
}

// A controller with no worker facts reads its disk's use itself when the
// panel sees the mount (not an installed panel).
func TestNodeDiskControllerReadsUseInProcess(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	mnt := "/dev/shm" // a real mount point; a plain folder is not one
	if _, _, ok := mountStatfs(mnt); !ok {
		t.Skip("no mounted tmpfs at /dev/shm")
	}
	must(t, n.orch.store.NodeDisks.Create(ctx, store.NodeDisk{ID: "dc", NodeID: n.ctl.ID, Kind: "path", MountPath: mnt, State: disk.StateReady}))
	if v, err := n.orch.NodeDisk(ctx, n.ctl.ID); err != nil || v.Total == 0 || v.Free == 0 || v.Stale {
		t.Errorf("NodeDisk = %+v, %v", v, err)
	}
	n.orch.cfg.PanelSpec = func(string) ContainerSpec { return ContainerSpec{} }
	n.orch.cfg.Version = "v1.2.3"
	if v, _ := n.orch.NodeDisk(ctx, n.ctl.ID); v.Total != 0 {
		t.Errorf("an installed panel read the host path: %+v", v)
	}
}

// The move plans use the same in-process read as the drawer, so they do not
// refuse "not read its disk yet" for a controller that shows Used.
func TestDataDiskOfControllerReadsTheMountInProcess(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	mnt := "/dev/shm"
	if _, _, ok := mountStatfs(mnt); !ok {
		t.Skip("no mounted tmpfs at /dev/shm")
	}
	must(t, n.orch.store.NodeDisks.Create(ctx, store.NodeDisk{ID: "dc", NodeID: n.ctl.ID, Kind: "path", MountPath: mnt, State: disk.StateReady}))
	if free, total, err := n.orch.dataDiskOf(ctx, n.ctl.ID); err != nil || total == 0 || free == 0 {
		t.Errorf("dataDiskOf = %d %d %v", free, total, err)
	}
	n.orch.cfg.PanelSpec = func(string) ContainerSpec { return ContainerSpec{} }
	n.orch.cfg.Version = "v1.2.3"
	if _, _, err := n.orch.dataDiskOf(ctx, n.ctl.ID); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("an installed panel read the host path: %v", err)
	}
}
