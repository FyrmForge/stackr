package store_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

func disk(id, node string) store.NodeDisk {
	return store.NodeDisk{ID: id, NodeID: node, Kind: "path", MountPath: "/disk", State: "ready", CreatedAt: t0}
}

func TestNodeDiskRoundTrip(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()
	if _, err := s.NodeDisks.GetByNode(ctx, "n1"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("GetByNode on a node without a disk = %v, want ErrNotFound", err)
	}
	if err := s.NodeDisks.Create(ctx, disk("d1", "n1")); err != nil {
		t.Fatal(err)
	}
	got, err := s.NodeDisks.GetByNode(ctx, "n1")
	if err != nil || got.ID != "d1" || got.OpNodeID != "" || got.OpFromNodeID != "" || got.OpMountPath != "" || got.OpEnvIDs != "" || !got.CreatedAt.Equal(t0) {
		t.Fatalf("GetByNode = %+v, %v", got, err)
	}
	got.State = "missing"
	got.OpNodeID = "n2"
	got.OpEnvIDs = `["e1","e2"]`
	if err := s.NodeDisks.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.NodeDisks.Get(ctx, "d1"); again.State != "missing" || again.OpNodeID != "n2" || again.OpEnvIDs != `["e1","e2"]` {
		t.Errorf("after Update: %+v", again)
	}
	if rows, err := s.NodeDisks.List(ctx); err != nil || len(rows) != 1 {
		t.Errorf("List = %d rows, %v", len(rows), err)
	}
}

// One disk per node: a second row for the same node is a conflict.
func TestNodeDiskNodeIDUnique(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()
	if err := s.NodeDisks.Create(ctx, disk("d1", "n1")); err != nil {
		t.Fatal(err)
	}
	if _, ok := errs.IsConflict(s.NodeDisks.Create(ctx, disk("d2", "n1"))); !ok {
		t.Error("second disk for one node was not a conflict")
	}
}

// kind carries no CHECK: the service validates it in Go.
func TestNodeDiskKindNotConstrained(t *testing.T) {
	s := servicetest.Store(t)
	d := disk("d1", "n1")
	d.Kind = "zfs-someday"
	if err := s.NodeDisks.Create(context.Background(), d); err != nil {
		t.Fatalf("an unknown kind was refused by SQL: %v", err)
	}
}

func TestDiskFoldersCascadeAndUnique(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()
	for _, id := range []string{"d1", "d2"} {
		if err := s.NodeDisks.Create(ctx, disk(id, "n-"+id)); err != nil {
			t.Fatal(err)
		}
	}
	f := func(id, disk, name string) store.DiskFolder {
		return store.DiskFolder{ID: id, DiskID: disk, Name: name, CreatedAt: t0}
	}
	for _, r := range []store.DiskFolder{f("f1", "d1", "b"), f("f2", "d1", "a"), f("f3", "d2", "a")} {
		if err := s.DiskFolders.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := errs.IsConflict(s.DiskFolders.Create(ctx, f("f4", "d1", "a"))); !ok {
		t.Error("same name twice on one disk was not a conflict")
	}
	rows, err := s.DiskFolders.ListByDisk(ctx, "d1")
	if err != nil || len(rows) != 2 || rows[0].Name != "a" {
		t.Fatalf("ListByDisk(d1) = %+v, %v", rows, err)
	}
	if err := s.DiskFolders.Delete(ctx, "f2"); err != nil {
		t.Fatal(err)
	}
	if err := s.NodeDisks.Delete(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.DiskFolders.ListByDisk(ctx, "d1"); len(rows) != 0 {
		t.Errorf("folders of a deleted disk survived: %+v", rows)
	}
	if rows, _ := s.DiskFolders.ListByDisk(ctx, "d2"); len(rows) != 1 {
		t.Errorf("another disk's folders changed: %+v", rows)
	}
}

// 017 down drops both tables and up brings them back, with rows in the
// tables it does not touch left alone.
func TestNodeDisksMigrationDownThenUp(t *testing.T) {
	s := servicetest.Store(t)
	db := s.DB()
	ctx := context.Background()
	db.MustExec(`INSERT INTO settings (key, value) VALUES ('k', 'v')`)
	if err := s.NodeDisks.Create(ctx, disk("d1", "n1")); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, err := os.ReadFile("../../../db/migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	db.MustExec(read("017_node_disks.down.sql"))
	if _, err := s.NodeDisks.List(ctx); err == nil {
		t.Fatal("node_disks still there after down")
	}
	db.MustExec(read("017_node_disks.up.sql"))
	if rows, err := s.NodeDisks.List(ctx); err != nil || len(rows) != 0 {
		t.Errorf("after up again: %d rows, %v", len(rows), err)
	}
	var n int
	if err := db.Get(&n, `SELECT count(*) FROM settings WHERE key = 'k'`); err != nil || n != 1 {
		t.Errorf("a table 017 does not touch lost its row: %d, %v", n, err)
	}
}
