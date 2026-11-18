package admin

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
)

func xfsPanel(v service.NodeDiskView) service.NodeDiskPanel {
	v.Has, v.Kind, v.State = true, "xfs", "ready"
	if v.Device == "" {
		v.Device = "ata-Samsung_1"
	}
	return service.NodeDiskPanel{NodeDiskView: v}
}

// 18a: new space shows Use it; none, a damaged disk or an offline node shows
// no row.
func TestXfsViewNewSpace(t *testing.T) {
	grown := service.NodeDiskView{DeviceBytes: 700 << 30, WasBytes: 500 << 30, NewSpace: 200 << 30}
	var dk ui.NodeDisk
	xfsView(&dk, xfsPanel(grown), "/n", true)
	if dk.NewSpace != "200 GB" || dk.Grow != "/n/disk/grow" || dk.DeviceSize != "700 GB" || dk.Damaged != "" {
		t.Errorf("new space: %+v", dk)
	}
	damaged := grown
	damaged.Damaged = "other_fs"
	for name, p := range map[string]service.NodeDiskPanel{
		"none":    xfsPanel(service.NodeDiskView{DeviceBytes: 500 << 30, WasBytes: 500 << 30}),
		"damaged": xfsPanel(damaged),
	} {
		dk = ui.NodeDisk{}
		xfsView(&dk, p, "/n", true)
		if dk.Grow != "" || dk.NewSpace != "" {
			t.Errorf("%s: Use it offered: %+v", name, dk)
		}
	}
	dk = ui.NodeDisk{}
	xfsView(&dk, xfsPanel(grown), "/n", false)
	if dk.Grow != "" {
		t.Error("Use it on an offline node")
	}
}

// 18f, 18g: a device that shrank names both sizes; another filesystem says so.
func TestXfsViewDamaged(t *testing.T) {
	var dk ui.NodeDisk
	xfsView(&dk, xfsPanel(service.NodeDiskView{Device: "/dev/sdb", DeviceBytes: 300 << 30, WasBytes: 500 << 30, Damaged: "smaller"}), "/n", true)
	const want = "sdb is now 300 GB, was 500 GB. Data on it may be damaged. Make the device 500 GB again, then check the disk."
	if dk.Damaged != want || dk.DeviceWas != "500 GB" {
		t.Errorf("smaller: %q %+v", dk.Damaged, dk)
	}
	dk = ui.NodeDisk{}
	xfsView(&dk, xfsPanel(service.NodeDiskView{Device: "/dev/sdb", DeviceBytes: 500 << 30, WasBytes: 500 << 30, Damaged: "other_fs"}), "/n", true)
	if dk.Damaged != "Another filesystem is on sdb now. The disk's data may be gone." {
		t.Errorf("other_fs: %q", dk.Damaged)
	}
}
