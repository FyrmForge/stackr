package disk_test

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
)

func TestOfNodeFindsAnAdoptingRowFromTheTargetNode(t *testing.T) {
	r := newRig(t)
	d := r.ready() // n1's disk
	if _, ok, _ := r.l.OfNode(ctx, "n2"); ok {
		t.Fatal("n2 has no disk yet")
	}
	d.State, d.OpNodeID, d.OpFromNodeID, d.OpMountPath = disk.StateAdopting, "n2", "n1", "/disk"
	if err := r.st.NodeDisks.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, ok, err := r.l.OfNode(ctx, "n2")
	if err != nil || !ok || got.ID != d.ID || got.State != disk.StateAdopting {
		t.Fatalf("OfNode(n2) = %+v, %v, %v", got, ok, err)
	}
	if _, ok, _ := r.l.OfNode(ctx, "n3"); ok {
		t.Error("an unrelated node found the row")
	}
	// a ready row names no target, whatever op_node_id says
	d.State = disk.StateReady
	if err := r.st.NodeDisks.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := r.l.OfNode(ctx, "n2"); ok {
		t.Error("a ready row was found from op_node_id")
	}
}

// DL: the operation context opens a disk only for its own disk and only
// while the row is in the state the context names.
func TestOpContextIsHonouredOnlyForItsOwnDiskAndState(t *testing.T) {
	r := newRig(t)
	d := r.ready()
	d.State = disk.StateAdopting
	if err := r.st.NodeDisks.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := r.l.Place(ctx, d, "a"); !isMissing(err) {
		t.Fatalf("plain ctx on adopting: %v", err)
	}
	if _, err := r.l.Place(disk.WithOp(ctx, "other", disk.StateAdopting), d, "a"); !isMissing(err) {
		t.Errorf("another disk's op: %v", err)
	}
	if _, err := r.l.Place(disk.WithOp(ctx, d.ID, disk.StateForgetting), d, "a"); !isMissing(err) {
		t.Errorf("the wrong mode: %v", err)
	}
	if _, err := r.l.Place(disk.WithOp(ctx, d.ID, "ready"), d, "a"); !isMissing(err) {
		t.Errorf("an invented mode: %v", err)
	}
	if _, err := r.l.Place(disk.WithoutOp(disk.WithOp(ctx, d.ID, disk.StateAdopting)), d, "a"); !isMissing(err) {
		t.Errorf("a dropped op: %v", err)
	}
	if _, err := r.l.Place(disk.WithOp(ctx, d.ID, disk.StateAdopting), d, "a"); err != nil || !r.exists(d.ID+"/a") {
		t.Fatalf("the adopt's own op: %v", err)
	}
	if _, err := r.l.Usage(disk.WithOp(ctx, d.ID, disk.StateAdopting), d, false, "a"); err != nil {
		t.Errorf("usage under the op: %v", err)
	}
	// forgetting never opens the leaf: the forget job works on plain volumes
	d.State = disk.StateForgetting
	if err := r.st.NodeDisks.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := r.l.Place(disk.WithOp(ctx, d.ID, disk.StateForgetting), d, "b"); !isMissing(err) {
		t.Errorf("forgetting op opened the leaf: %v", err)
	}
	// an op context never outlives the state it named
	if _, err := r.l.Place(disk.WithOp(ctx, d.ID, disk.StateAdopting), d, "b"); !isMissing(err) {
		t.Errorf("a stale adopting op on a forgetting row: %v", err)
	}
}
