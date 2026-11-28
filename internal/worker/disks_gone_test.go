package worker

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

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
		d := "sys/dev/block"
		if c.inSysfs {
			d += "/8:16"
		}
		if err := os.MkdirAll(filepath.Join(s.cfg.HostRoot, d), 0o755); err != nil {
			t.Fatal(err)
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

func TestDevGone(t *testing.T) {
	root := t.TempDir()
	if devGone(root, "8:16") {
		t.Error("no sys/dev/block: must not read as gone")
	}
	blk := filepath.Join(root, "sys/dev/block")
	if err := os.MkdirAll(blk, 0o755); err != nil {
		t.Fatal(err)
	}
	if !devGone(root, "8:16") {
		t.Error("entry absent: gone")
	}
	if err := os.Symlink("../../devices/x", filepath.Join(blk, "8:16")); err != nil {
		t.Fatal(err)
	}
	if devGone(root, "8:16") {
		t.Error("entry present: not gone")
	}
	// a relative root is joined from /, not the working dir
	if devGone("", "8:16") != devGone("/", "8:16") {
		t.Error("empty root differs from /")
	}
}

// A read hung for 3 intervals reports IOErr with a ReadAt that moves once per
// interval; a path or network disk hung the same way stays stale.
func TestDisksFactsHungRead(t *testing.T) {
	for _, c := range []struct {
		name, table, device  string
		inSysfs, ioErr, gone bool
	}{
		{"device pulled", rootLine + dataLine, "dev-1", false, true, true},
		{"device present", rootLine + dataLine, "dev-1", true, true, false},
		{"path disk", rootLine + dataLine, "", false, false, false},
		{"network", rootLine + "3 1 0:40 / /mnt/data rw - nfs4 srv:/x rw\n", "dev-1", false, false, false},
	} {
		s := diskServer(t, c.table, hang)
		d := "sys/dev/block"
		if c.inSysfs {
			d += "/8:16"
		}
		if err := os.MkdirAll(filepath.Join(s.cfg.HostRoot, d), 0o755); err != nil {
			t.Fatal(err)
		}
		reg := regA
		reg.Device = c.device
		if err := s.PutDisks([]DiskReg{reg}); err != nil {
			t.Fatal(err)
		}
		eventually(t, c.name+": read in flight", func() bool {
			s.dmu.Lock()
			defer s.dmu.Unlock()
			return s.disks.ents[reg.Path].running
		})
		s.dmu.Lock()
		s.disks.ents[reg.Path].started = time.Now().Add(-3*time.Hour - time.Minute)
		s.dmu.Unlock()
		a, b := facts(s).Disks[0], facts(s).Disks[0]
		if a.IOErr != c.ioErr || a.DeviceGone != c.gone {
			t.Errorf("%s: IOErr %v DeviceGone %v, want %v %v", c.name, a.IOErr, a.DeviceGone, c.ioErr, c.gone)
		}
		if c.ioErr && (!a.ReadAt.Equal(b.ReadAt) || a.ReadAt.IsZero()) {
			t.Errorf("%s: ReadAt moved between calls: %v %v", c.name, a.ReadAt, b.ReadAt)
		}
	}
}
