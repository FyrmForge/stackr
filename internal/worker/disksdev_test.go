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
	s := diskServer(t, rootLine+dataLine, func(context.Context, string) (hostdisk.Stamp, uint64, uint64, error) {
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
