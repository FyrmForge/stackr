package disk_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

const (
	rootLine = "25 1 8:1 / / rw - ext4 /dev/sda1 rw\n"
	diskLine = "36 25 8:17 / /disk rw - ext4 /dev/sdb rw\n"
)

// rig: the fake Host is hostdisk.Run on a temp root.
type rig struct {
	t     *testing.T
	st    *store.Store
	l     *disk.Leaf
	env   hostdisk.Env
	dir   string // the real dir behind /disk
	calls []string
	mu    sync.Mutex
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	r := &rig{t: t, st: servicetest.Store(t), dir: filepath.Join(root, "disk")}
	if err := os.Mkdir(r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.env = hostdisk.Env{Root: root, Mountinfo: filepath.Join(t.TempDir(), "mountinfo")}
	r.table(rootLine + diskLine)
	r.l = disk.New(r.st.NodeDisks, r.st.DiskFolders, r)
	return r
}

func (r *rig) Disk(ctx context.Context, _ string, req hostdisk.Request) (hostdisk.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req.Op)
	r.mu.Unlock()
	return hostdisk.Run(ctx, r.env, req), nil
}

func (r *rig) table(s string) {
	r.t.Helper()
	dev, err := hostdisk.DevString(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	s = strings.ReplaceAll(s, " 8:17 ", " "+dev+" ") // the fake device becomes the real one
	if err := os.WriteFile(r.env.Mountinfo, []byte(s), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) ready() store.NodeDisk {
	r.t.Helper()
	d, err := r.l.AddPath(ctx, "n1", "/disk", "i1")
	if err != nil {
		r.t.Fatal(err)
	}
	return d
}

func (r *rig) exists(p string) bool {
	_, err := os.Lstat(filepath.Join(r.dir, p))
	return err == nil
}

func (r *rig) state(id string) string {
	r.t.Helper()
	d, err := r.st.NodeDisks.Get(ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return d.State
}

func (r *rig) folders(d store.NodeDisk) int {
	r.t.Helper()
	rows, err := r.st.DiskFolders.ListByDisk(ctx, d.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return len(rows)
}

func isMissing(err error) bool {
	var m *disk.MissingError
	return errors.As(err, &m) && errors.Is(err, errs.ErrBusy)
}

func code(err error) string {
	var he *hostdisk.Error
	if errors.As(err, &he) {
		return he.Code
	}
	return ""
}

func TestAddPathRowAddingBeforeStampThenReady(t *testing.T) {
	r := newRig(t)
	// A host that refuses to run still leaves the row in adding when the call
	// is a crash (a transport error), because the row is written first.
	crash := disk.New(r.st.NodeDisks, r.st.DiskFolders, crashHost{})
	if _, err := crash.AddPath(ctx, "n1", "/disk", "i1"); err == nil {
		t.Fatal("expected the crash")
	}
	row, ok, _ := r.l.OfNode(ctx, "n1")
	if !ok || row.State != disk.StateAdding || r.exists(".stackr-disk") {
		t.Fatalf("after crash: %+v ok=%v stamp=%v", row, ok, r.exists(".stackr-disk"))
	}
	got, err := r.l.AddPath(ctx, "n1", "/disk", "i1")
	if err != nil || got.ID != row.ID || got.State != disk.StateReady {
		t.Fatalf("retry = %+v, %v", got, err)
	}
	if !r.exists(".stackr-disk") || !r.exists(got.ID) || r.state(got.ID) != disk.StateReady {
		t.Fatal("stamp, id folder or ready state missing")
	}
}

type crashHost struct{}

func (crashHost) Disk(context.Context, string, hostdisk.Request) (hostdisk.Result, error) {
	return hostdisk.Result{}, errors.New("helper crashed")
}

func TestReaddAcceptsAStampMatchingAnAddingRowOfThisNode(t *testing.T) {
	r := newRig(t)
	d := r.ready()
	d.State = disk.StateAdding
	if err := r.st.NodeDisks.Update(ctx, d); err != nil { // as if the crash came after the stamp
		t.Fatal(err)
	}
	got, err := r.l.AddPath(ctx, "n1", "/disk", "i1")
	if err != nil || got.ID != d.ID || got.State != disk.StateReady {
		t.Fatalf("re-add = %+v, %v", got, err)
	}
}

// DL: a stamp that belongs to no row of this node is never adopted by add.
func TestReaddRefusesAStampMatchingNoRow(t *testing.T) {
	r := newRig(t)
	old := r.ready()
	if err := r.l.Delete(ctx, old); err != nil { // rows gone, stamp stays
		t.Fatal(err)
	}
	_, err := r.l.AddPath(ctx, "n1", "/disk", "i1")
	if code(err) != hostdisk.CodeForeignStamp {
		t.Fatalf("add = %v, want foreign_stamp", err)
	}
	if _, ok, _ := r.l.OfNode(ctx, "n1"); ok {
		t.Fatal("a refused add left a row")
	}
	if !r.exists(old.ID) {
		t.Fatal("the old id folder vanished")
	}
}

func TestPlaceRowBeforeFolder(t *testing.T) {
	r := newRig(t)
	d := r.ready()
	opts, err := r.l.Place(ctx, d, "vol1")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/disk", d.ID, "vol1")
	if opts["device"] != want || opts["o"] != "bind" || opts["type"] != "none" {
		t.Fatalf("opts = %v, want device %s", opts, want)
	}
	if r.folders(d) != 1 || !r.exists(d.ID+"/vol1") {
		t.Fatal("row or folder missing")
	}
	// The folder is refused: the row this call made goes with it.
	r.table(rootLine)
	if _, err := r.l.Place(ctx, store.NodeDisk{ID: d.ID, NodeID: "n1"}, "vol2"); !isMissing(err) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := r.l.HasFolders(ctx, d, "vol1", "vol2"); !got["vol1"] || got["vol2"] {
		t.Fatalf("has = %v", got)
	}
}

func TestPlaceIsGetOrCreateUnderConcurrentCalls(t *testing.T) {
	r := newRig(t)
	d := r.ready()
	var wg sync.WaitGroup
	errc := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.l.Place(ctx, d, "vol1")
			errc <- err
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			t.Fatalf("a concurrent Place failed: %v", err)
		}
	}
	if r.folders(d) != 1 || !r.exists(d.ID+"/vol1") {
		t.Fatalf("rows = %d", r.folders(d))
	}
	if _, err := r.l.Place(ctx, d, "vol1"); err != nil {
		t.Fatalf("second Place = %v", err)
	}
}

// DL: a vanished mount latches missing and nothing is made, not on the root
// filesystem and not as a row.
func TestPlaceOnMissingDiskLatchesMissingAndMakesNothing(t *testing.T) {
	r := newRig(t)
	d := r.ready()
	r.table(rootLine) // unmounted
	if _, err := r.l.Place(ctx, d, "vol1"); !isMissing(err) {
		t.Fatalf("err = %v, want *MissingError", err)
	}
	if r.state(d.ID) != disk.StateMissing {
		t.Fatalf("state = %s", r.state(d.ID))
	}
	if r.folders(d) != 0 || r.exists(d.ID+"/vol1") {
		t.Fatal("something was made")
	}
	// Remounted, the latch still holds until Verify: nothing reaches the host.
	r.table(rootLine + diskLine)
	n := len(r.calls)
	if _, err := r.l.Place(ctx, d, "vol1"); !isMissing(err) || len(r.calls) != n {
		t.Fatalf("latched Place = %v, host calls %d -> %d", err, n, len(r.calls))
	}
}

// DL: any non-ready state refuses before the host is reached.
func TestPlaceUsageDropOnAddingDiskReturnMissing(t *testing.T) {
	for _, state := range []string{disk.StateAdding, disk.StateMissing} {
		r := newRig(t)
		d := r.ready()
		d.State = state
		if err := r.st.NodeDisks.Update(ctx, d); err != nil {
			t.Fatal(err)
		}
		n := len(r.calls)
		_, e1 := r.l.Place(ctx, d, "v")
		_, e2 := r.l.Usage(ctx, d, false, "v")
		e3 := r.l.Drop(ctx, d, "v")
		for _, err := range []error{e1, e2, e3} {
			if !isMissing(err) {
				t.Fatalf("%s: err = %v", state, err)
			}
		}
		if len(r.calls) != n || r.folders(d) != 0 {
			t.Fatalf("%s: the host was reached or a row made", state)
		}
	}
}

func TestDropRemovesFolderThenRow(t *testing.T) {
	r := newRig(t)
	d := r.ready()
	if _, err := r.l.Place(ctx, d, "vol1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, d.ID, "vol1", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.l.Drop(ctx, d, "vol1"); err != nil {
		t.Fatal(err)
	}
	if r.exists(d.ID+"/vol1") || r.folders(d) != 0 {
		t.Fatal("folder or row left")
	}
	if err := r.l.Drop(ctx, d, "vol1"); err != nil { // already gone
		t.Fatalf("second drop = %v", err)
	}
}

// DL: a disk that is not there keeps the folder row, so the data is still
// accounted for when the disk comes back.
func TestDropOnMissingDiskKeepsRow(t *testing.T) {
	r := newRig(t)
	d := r.ready()
	if _, err := r.l.Place(ctx, d, "vol1"); err != nil {
		t.Fatal(err)
	}
	r.table(rootLine)
	if err := r.l.Drop(ctx, d, "vol1"); !isMissing(err) {
		t.Fatalf("err = %v", err)
	}
	if r.folders(d) != 1 || !r.exists(d.ID+"/vol1") {
		t.Fatal("row or folder lost")
	}
}

// DL: a failed usage read is an error, never an empty answer that reads as
// "no data here".
func TestUsageErrorIsErrorNotZero(t *testing.T) {
	r := newRig(t)
	d := r.ready()
	if _, err := r.l.Place(ctx, d, "vol1"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.l.Usage(ctx, d, false, "vol1", "nope"); err != nil || !got["vol1"].Empty || !got["nope"].Missing {
		t.Fatalf("usage = %v, %v", got, err)
	}
	r.table(rootLine)
	if got, err := r.l.Usage(ctx, d, true, "vol1"); err == nil || got != nil {
		t.Fatalf("usage on a gone disk = %v, %v", got, err)
	}
	d2 := r.ready2(t)
	h := disk.New(r.st.NodeDisks, r.st.DiskFolders, crashHost{})
	if got, err := h.Usage(ctx, d2, false, "x"); err == nil || got != nil {
		t.Fatalf("usage on a crashed host = %v, %v", got, err)
	}
}

// ready2 is the row as the store holds it, ready, for a host that crashes.
func (r *rig) ready2(t *testing.T) store.NodeDisk {
	d, _, _ := r.l.OfNode(ctx, "n1")
	d.State = disk.StateReady
	if err := r.st.NodeDisks.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestVerifySetsReady(t *testing.T) {
	r := newRig(t)
	d := r.ready()
	r.table(rootLine)
	if got, err := r.l.Verify(ctx, d); err != nil || got.State != disk.StateMissing {
		t.Fatalf("verify unmounted = %+v, %v", got, err)
	}
	r.table(rootLine + diskLine)
	if got, err := r.l.Verify(ctx, d); err != nil || got.State != disk.StateReady || r.state(d.ID) != disk.StateReady {
		t.Fatalf("verify remounted = %+v, %v", got, err)
	}
	// A state that carries an unfinished operation is never moved.
	d.State = "adopting"
	if err := r.st.NodeDisks.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	r.table(rootLine)
	if got, err := r.l.Verify(ctx, d); err != nil || got.State != "adopting" {
		t.Fatalf("verify adopting = %+v, %v", got, err)
	}
	if got, err := r.l.MarkMissing(ctx, d); err != nil || got.State != "adopting" {
		t.Fatalf("mark missing adopting = %+v, %v", got, err)
	}
}

// V4: a half-made add does not block the node for good.
func TestAddingRowAtAnotherPathIsReplacedByANewAdd(t *testing.T) {
	r := newRig(t)
	crash := disk.New(r.st.NodeDisks, r.st.DiskFolders, crashHost{})
	if _, err := crash.AddPath(ctx, "n1", "/old", "i1"); err == nil {
		t.Fatal("expected the crash")
	}
	old, _, _ := r.l.OfNode(ctx, "n1")
	got, err := r.l.AddPath(ctx, "n1", "/disk", "i1")
	if err != nil || got.MountPath != "/disk" || got.ID == old.ID || got.State != disk.StateReady {
		t.Fatalf("add = %+v, %v", got, err)
	}
	if _, err := r.st.NodeDisks.Get(ctx, old.ID); err == nil {
		t.Fatal("the old adding row is still there")
	}
}

func TestRefusedRetryDeletesTheAddingRowButNeverAReadyOne(t *testing.T) {
	r := newRig(t)
	crash := disk.New(r.st.NodeDisks, r.st.DiskFolders, crashHost{})
	if _, err := crash.AddPath(ctx, "n1", "/disk", "i1"); err == nil {
		t.Fatal("expected the crash")
	}
	if err := os.WriteFile(filepath.Join(r.dir, "x"), []byte("data"), 0o644); err != nil { // no longer empty
		t.Fatal(err)
	}
	if _, err := r.l.AddPath(ctx, "n1", "/disk", "i1"); code(err) != hostdisk.CodeNotEmpty {
		t.Fatalf("add = %v, want not_empty", err)
	}
	if _, ok, _ := r.l.OfNode(ctx, "n1"); ok {
		t.Fatal("a refused retry left the adding row")
	}
	// a ready row with the same refusal path stays
	if err := os.Remove(filepath.Join(r.dir, "x")); err != nil {
		t.Fatal(err)
	}
	row := r.ready()
	if _, err := r.l.AddPath(ctx, "n1", "/other", "i1"); !isConflict(err) {
		t.Fatalf("add over a ready row = %v", err)
	}
	if r.state(row.ID) != disk.StateReady {
		t.Fatal("the ready row changed")
	}
}

func isConflict(err error) bool { _, ok := errs.IsConflict(err); return ok }
