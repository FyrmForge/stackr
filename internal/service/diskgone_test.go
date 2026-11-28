package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
)

// A pulled disk (mount listed, device gone from sysfs, every read EIO) latches
// missing after 3 fresh reads and queues the quarantine, not erroring.
func TestDeviceGoneLatchesMissingAndQuarantines(t *testing.T) {
	d := newDiskTest(t)
	row := d.addDisk(t)
	f := ioRead(row)
	f.Disks[0].DeviceGone = true
	for range diskMissingReads {
		d.watch(t, f)
	}
	if st := d.stateOf(t, row); st != disk.StateMissing {
		t.Errorf("state = %q, want missing", st)
	}
	if erroringNow(d) {
		t.Error("a gone device latched erroring")
	}
	if len(d.quarJobs(t)) == 0 {
		t.Error("no disk-quarantine job was queued")
	}
}

// An erroring disk shows in NodeDisk.
func TestNodeDiskReportsErroring(t *testing.T) {
	d := newDiskTest(t)
	row := d.addDisk(t)
	if v, err := d.orch.NodeDisk(context.Background(), "w1"); err != nil || v.Erroring {
		t.Fatalf("healthy: %+v, %v", v, err)
	}
	for range diskMissingReads {
		d.watch(t, ioRead(row))
	}
	if v, err := d.orch.NodeDisk(context.Background(), "w1"); err != nil || !v.Erroring {
		t.Errorf("erroring: %+v, %v", v, err)
	}
}
