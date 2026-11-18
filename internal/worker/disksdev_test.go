package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/FyrmForge/stackr/internal/hostdisk"
)

// An xfs registration gets its device size and filesystem uuid from sysfs in
// the background refresh, and an old registration without a device gets none.
func TestDisksFactsCarryTheDeviceOfAnXFSDisk(t *testing.T) {
	s := diskServer(t, rootLine+"2 1 8:16 / /mnt/data rw - xfs /dev/sdb rw\n", func(context.Context, string) (hostdisk.Stamp, uint64, uint64, error) {
		return hostdisk.Stamp{ID: "d1"}, 5, 10, nil
	})
	r := s.cfg.HostRoot
	for _, d := range []string{"dev/disk/by-id", "dev/disk/by-uuid", "sys/class/block/sdb"} {
		if err := os.MkdirAll(filepath.Join(r, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range [][2]string{{"../../sdb", "dev/disk/by-id/ata-X"}, {"../../sdb", "dev/disk/by-uuid/uuid-1"}} {
		if err := os.Symlink(l[0], filepath.Join(r, l[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(r, "sys/class/block/sdb/size"), []byte("2097152\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := DiskReg{ID: "d2", Path: "/mnt/other", Owner: "n1"}
	if err := s.PutDisks([]DiskReg{{ID: "d1", Path: "/mnt/data", Owner: "n1", Device: "/dev/disk/by-id/ata-X"}, old}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "device facts", func() bool { return facts(s).Disks[0].DeviceBytes != 0 })
	d := facts(s).Disks[0]
	if d.Device != "/dev/disk/by-id/ata-X" || d.DeviceBytes != 1<<30 || d.FSUUID != "uuid-1" {
		t.Fatalf("device facts: %+v", d)
	}
	if o := facts(s).Disks[1]; o.Device != "" || o.DeviceBytes != 0 || o.FSUUID != "" {
		t.Fatalf("an old registration got device facts: %+v", o)
	}
}

// X8: with no by-id link the recorded name is a kernel name that can be another disk after a reboot.
// The facts follow the device the mount stands on, so a moved name is no false alarm.
func TestDisksFactsFollowTheMountNotTheRecordedName(t *testing.T) {
	s := diskServer(t, rootLine+"2 1 8:32 / /mnt/data rw - xfs /dev/vdc rw\n", func(context.Context, string) (hostdisk.Stamp, uint64, uint64, error) {
		return hostdisk.Stamp{ID: "d1"}, 5, 10, nil
	})
	r := s.cfg.HostRoot
	for _, d := range []string{"dev/disk/by-uuid", "sys/class/block/vdb", "sys/class/block/vdc"} {
		if err := os.MkdirAll(filepath.Join(r, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// vdb is now some other, smaller disk with another filesystem; the data disk is vdc
	for _, l := range [][2]string{{"../../vdb", "dev/disk/by-uuid/other-fs"}, {"../../vdc", "dev/disk/by-uuid/uuid-1"}} {
		if err := os.Symlink(l[0], filepath.Join(r, l[1])); err != nil {
			t.Fatal(err)
		}
	}
	for n, sectors := range map[string]string{"vdb": "1024\n", "vdc": "2097152\n"} {
		if err := os.WriteFile(filepath.Join(r, "sys/class/block/"+n+"/size"), []byte(sectors), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutDisks([]DiskReg{{ID: "d1", Path: "/mnt/data", Owner: "n1", Device: "/dev/vdb"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "device facts", func() bool { return facts(s).Disks[0].DeviceBytes != 0 })
	if d := facts(s).Disks[0]; d.DeviceBytes != 1<<30 || d.FSUUID != "uuid-1" {
		t.Fatalf("the facts followed the recorded name: %+v", d)
	}
}
