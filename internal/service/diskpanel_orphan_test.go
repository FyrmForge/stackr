package service

import (
	"context"
	"testing"
)

// An orphaned volume still holds its folder, so the disk lists it.
func TestNodeDiskPanelListsOrphanedVolumes(t *testing.T) {
	d := newDiskTest(t)
	d.addDisk(t)
	e, v := d.scratch(t, "lost")
	d.ensure(t, v)
	d.orphanOf(t, e, v)
	p, err := d.orch.NodeDiskPanel(context.Background(), "w1")
	must(t, err)
	if len(p.Volumes) != 1 || p.Volumes[0].Slug != "lost" || !p.Volumes[0].Orphaned {
		t.Fatalf("volumes = %+v, want the orphan listed", p.Volumes)
	}
}

// A path that is not a mount point is not a disk.
func TestMountStatfsNeedsAMountPoint(t *testing.T) {
	if _, _, ok := mountStatfs(t.TempDir()); ok {
		t.Error("a plain folder read as a mount")
	}
	if _, _, ok := mountStatfs("/"); ok {
		t.Error("/ has no parent device to differ from")
	}
}
