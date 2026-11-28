package worker

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/FyrmForge/stackr/internal/hostdisk"
)

// An EIO read of an xfs disk whose mount is still listed is DeviceGone when
// the device left sysfs, and a plain I/O error while it is still there. A
// path disk (no Device) is never DeviceGone.
func TestDisksFactsDeviceGone(t *testing.T) {
	for _, c := range []struct {
		name    string
		device  string
		inSysfs bool
		want    bool
	}{{"pulled", "dev-1", false, true}, {"present", "dev-1", true, false}, {"path disk", "", false, false}} {
		s := diskServer(t, rootLine+dataLine, func(context.Context, string) (hostdisk.Stamp, uint64, uint64, error) {
			return hostdisk.Stamp{}, 0, 0, syscall.EIO
		})
		if c.inSysfs {
			if err := os.MkdirAll(filepath.Join(s.cfg.HostRoot, "sys/dev/block/8:16"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		reg := regA
		reg.Device = c.device
		if err := s.PutDisks([]DiskReg{reg}); err != nil {
			t.Fatal(err)
		}
		eventually(t, c.name+": io error read", func() bool { return facts(s).Disks[0].IOErr })
		if got := facts(s).Disks[0].DeviceGone; got != c.want {
			t.Errorf("%s: DeviceGone = %v, want %v", c.name, got, c.want)
		}
	}
}
