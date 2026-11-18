package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/worker"
)

// The registry the panel pushes names the device of an xfs disk, so the worker
// can report its size and filesystem uuid.
func TestXFSRegistryCarriesTheDevice(t *testing.T) {
	x := newXFSTest(t)
	row := x.addXFS(t)
	regs, err := x.orch.diskRegs(context.Background(), "w1")
	must(t, err)
	if len(regs) != 1 || regs[0].Device == "" || regs[0].Device != row.DeviceID {
		t.Fatalf("regs = %+v, row device %q", regs, row.DeviceID)
	}
}

// A damaged xfs disk (a smaller device, another filesystem) stops every deploy
// to it: the folder is never placed and nothing reaches the host.
func TestXFSDamagedDiskStopsDeploys(t *testing.T) {
	ctx := context.Background()
	for name, f := range map[string]struct {
		bytes int64
		uuid  string
	}{"smaller": {1 << 29, "fs-uuid-0001"}, "other fs": {1 << 30, "other"}} {
		t.Run(name, func(t *testing.T) {
			x := newXFSTest(t)
			row := x.addXFS(t)
			x.facts(t, worker.Facts{DisksRead: true, Disks: []hostdisk.Fact{{Path: row.MountPath, Mounted: true, Free: 1, Total: 1 << 30,
				ReadAt: time.Now(), Device: row.DeviceID, DeviceBytes: f.bytes, FSUUID: f.uuid}}})
			if f.uuid == "fs-uuid-0001" && row.FSUUID != f.uuid {
				t.Fatalf("row uuid %q", row.FSUUID)
			}
			v := x.vol(t, "data")
			before := len(x.fx.quotaCmds())
			_, err := x.orch.volumes.Ensure(ctx, v)
			var me *volume.MissingError
			if !errors.As(err, &me) || !strings.Contains(err.Error(), "device changed") {
				t.Fatalf("ensure on a damaged disk: %v", err)
			}
			if len(x.folders(t, row)) != 0 || len(x.fx.quotaCmds()) != before {
				t.Errorf("a folder was placed: %v", x.folders(t, row))
			}
			// the disk is fine again
			x.facts(t, worker.Facts{DisksRead: true, Disks: []hostdisk.Fact{{Path: row.MountPath, Mounted: true, Free: 1, Total: 1 << 30,
				ReadAt: time.Now(), Device: row.DeviceID, DeviceBytes: 1 << 30, FSUUID: row.FSUUID}}})
			x.ensure(t, v)
		})
	}
}

// A volume over its cap does not move to an xfs disk: refused before any copy.
func TestXFSMoveRefusedWhenTheVolumeHoldsMoreThanItsCap(t *testing.T) {
	ctx := context.Background()
	x, v, _ := xfsMoveRig(t) // cap 500 MB
	x.wFake.Volumes = []docker.VolumeInfo{{Name: v.Name, SizeBytes: 900 << 20}}
	j, err := x.orch.MoveVolumeToDisk(ctx, adminP(), v.ID)
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "Raise the cap first") || j.ID != "" {
		t.Fatalf("move = %+v, %v", j, err)
	}
	if len(x.fx.quotaCmds()) != 0 {
		t.Errorf("the host was touched: %v", x.fx.quotaCmds())
	}
}
