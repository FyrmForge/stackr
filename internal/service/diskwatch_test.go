package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

const gig = 1 << 30

// diskFacts is a worker's read of /disk: mounted and stamped with the row's id.
func readOf(row store.NodeDisk, free, total uint64) worker.Facts {
	return worker.Facts{DisksRead: true, Disks: []hostdisk.Fact{{
		Path: row.MountPath, Mounted: true, StampID: row.ID, Free: free, Total: total}}}
}

func goneOf(row store.NodeDisk) worker.Facts {
	f := readOf(row, 0, 0)
	f.Disks[0].Mounted = false
	return f
}

func (d *diskTest) watch(t *testing.T, f worker.Facts) {
	t.Helper()
	n, err := d.st.Nodes.Get(context.Background(), "w1")
	must(t, err)
	d.orch.diskWatch(context.Background(), n, f)
}

func (d *diskTest) stateOf(t *testing.T, row store.NodeDisk) string {
	t.Helper()
	got, err := d.st.NodeDisks.Get(context.Background(), row.ID)
	must(t, err)
	return got.State
}

func TestAlertAt80PctFiresOnceAndResets(t *testing.T) {
	d := newDiskTest(t)
	row := d.addDisk(t)
	h := newHook(t)
	alertsTo(t, d.orch, h, "")
	for i, s := range []struct {
		free  uint64
		fires int
	}{{50 * gig, 0}, {20 * gig, 1}, {10 * gig, 0}, {50 * gig, 0}, {19 * gig, 1}} {
		d.watch(t, readOf(row, s.free, 100*gig))
		if got := h.events(t, d.orch); len(got) != s.fires || (s.fires == 1 && got[0] != AlertDiskFull+":"+SevWarning) {
			t.Errorf("step %d: %v", i, got)
		}
	}
}

func TestMissingLatchesOnlyAfterNConsecutiveFreshReads(t *testing.T) {
	d := newDiskTest(t)
	row := d.addDisk(t)
	for i := 1; i <= diskMissingReads; i++ {
		if st := d.stateOf(t, row); st != disk.StateReady {
			t.Fatalf("after %d bad reads state = %q, want ready", i-1, st)
		}
		d.watch(t, goneOf(row))
	}
	if st := d.stateOf(t, row); st != disk.StateMissing {
		t.Errorf("after %d bad reads state = %q, want missing", diskMissingReads, st)
	}
}

func TestOneBadOrStaleReadDoesNotLatch(t *testing.T) {
	d := newDiskTest(t)
	row := d.addDisk(t)
	d.watch(t, goneOf(row))
	stale := goneOf(row)
	stale.DisksStale = true
	for range 2 * diskMissingReads {
		d.watch(t, stale)
	}
	// stale reads break the run: bad, stale, bad, bad is not three in a row
	d.watch(t, goneOf(row))
	d.watch(t, stale)
	d.watch(t, goneOf(row))
	d.watch(t, goneOf(row))
	// and a good read resets it
	d.watch(t, readOf(row, 50*gig, 100*gig))
	d.watch(t, goneOf(row))
	d.watch(t, goneOf(row))
	// a stamp of another disk counts as gone
	wrong := readOf(row, 1, 2)
	wrong.Disks[0].StampID = "other"
	d.watch(t, wrong)
	if st := d.stateOf(t, row); st != disk.StateMissing {
		t.Fatalf("state = %q: two bad plus a wrong stamp is three in a row", st)
	}
	row = d.freshDisk(t)
	d.watch(t, goneOf(row))
	d.watch(t, stale)
	if st := d.stateOf(t, row); st != disk.StateReady {
		t.Errorf("state = %q, want ready", st)
	}
}

// freshDisk puts the latched row back to ready the way a test may (Check disk
// is the product's only way) and clears the run.
func (d *diskTest) freshDisk(t *testing.T) store.NodeDisk {
	t.Helper()
	row, _, err := d.orch.nodeDisk(context.Background(), "w1")
	must(t, err)
	setState(t, d, row, disk.StateReady)
	row.State = disk.StateReady
	d.orch.alertWatch().diskMu.Lock()
	delete(d.orch.alertWatch().diskBad, "w1")
	d.orch.alertWatch().diskMu.Unlock()
	return row
}

func TestOneHelperVerifyDiskMissingLatches(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	must(t, os.Remove(filepath.Join(d.env.Root, "disk", ".stackr-disk")))
	got, err := d.orch.diskLeaf.Verify(ctx, row)
	must(t, err)
	if got.State != disk.StateMissing || d.stateOf(t, row) != disk.StateMissing {
		t.Errorf("state = %q, want missing after one verify", got.State)
	}
	h := newHook(t)
	alertsTo(t, d.orch, h, "")
	d.watch(t, readOf(row, 50*gig, 100*gig))
	if ev := h.events(t, d.orch); len(ev) != 1 || ev[0] != AlertDiskMissing+":"+SevCritical {
		t.Errorf("events = %v", ev)
	}
}

func TestMissingFiresCriticalAndLatches(t *testing.T) {
	d := newDiskTest(t)
	row := d.addDisk(t)
	h := newHook(t)
	alertsTo(t, d.orch, h, "")
	for range diskMissingReads {
		d.watch(t, goneOf(row))
	}
	if ev := h.events(t, d.orch); len(ev) != 1 || ev[0] != AlertDiskMissing+":"+SevCritical {
		t.Fatalf("events = %v", ev)
	}
	// once; and the disk coming back never makes it ready
	d.watch(t, goneOf(row))
	d.watch(t, readOf(row, 50*gig, 100*gig))
	if ev := h.events(t, d.orch); len(ev) != 0 {
		t.Errorf("fired again: %v", ev)
	}
	if st := d.stateOf(t, row); st != disk.StateMissing {
		t.Errorf("state = %q, want still missing", st)
	}
	if !d.orch.hasProblem(context.Background(), diskMissingKey("w1")) {
		t.Error("no open problem for the missing disk")
	}
}

func TestUnknownFactsNeverMarkMissing(t *testing.T) {
	d := newDiskTest(t)
	row := d.addDisk(t)
	old := goneOf(row)
	old.DisksRead = false // a worker too old to read disks
	noEntry := worker.Facts{DisksRead: true}
	other := goneOf(row)
	other.Disks[0].Path = "/elsewhere"
	for range 3 * diskMissingReads {
		d.watch(t, old)
		d.watch(t, noEntry)
		d.watch(t, other)
		d.watch(t, worker.Facts{})
	}
	if st := d.stateOf(t, row); st != disk.StateReady {
		t.Errorf("state = %q, want ready", st)
	}
}

// DL: a failed row lookup skips the pass: nothing latches, nothing fires, and
// the reads before it do not count after it.
func TestStateLookupErrorSkipsTheAlertTickAndLatchesNothing(t *testing.T) {
	d := newDiskTest(t)
	row := d.addDisk(t)
	h := newHook(t)
	alertsTo(t, d.orch, h, "")
	good := d.orch.diskLeaf
	d.watch(t, goneOf(row))
	d.orch.diskLeaf = disk.New(failDisks{d.st.NodeDisks}, d.st.DiskFolders, InProcessDisk(d.env))
	for range 3 * diskMissingReads {
		d.watch(t, goneOf(row))
	}
	d.orch.diskLeaf = good
	if st := d.stateOf(t, row); st != disk.StateReady {
		t.Fatalf("state = %q, want ready", st)
	}
	d.watch(t, goneOf(row)) // the pass that failed left the run at one
	if st := d.stateOf(t, row); st != disk.StateReady {
		t.Errorf("state = %q: reads during the lookup error counted", st)
	}
	if ev := h.events(t, d.orch); len(ev) != 0 {
		t.Errorf("events = %v", ev)
	}
}

func TestGateRefusesUnder1GBOnNodeDisk(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	d.facts(t, readOf(row, gig/2, 10*gig))
	_, err := d.orch.diskCheck(ctx, "w1")
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "its disk") {
		t.Fatalf("diskCheck err = %v", err)
	}
	d.facts(t, readOf(row, 5*gig, 10*gig))
	if _, err := d.orch.diskCheck(ctx, "w1"); err != nil {
		t.Errorf("5 GB free refused: %v", err)
	}
}

// DL: a latched disk answers *disk.MissingError, never the Docker disk's room.
func TestDataDiskOfOnLatchedDiskReturnsMissingAndNeverTheDockerDisk(t *testing.T) {
	d := newDiskTest(t)
	row := d.addDisk(t)
	f := readOf(row, 50*gig, 100*gig)
	f.DiskFree, f.DiskTotal = 80*gig, 100*gig // an ample Docker disk
	d.facts(t, f)
	setState(t, d, row, disk.StateMissing)
	free, total, err := d.orch.dataDiskOf(context.Background(), "w1")
	var me *disk.MissingError
	if !errors.As(err, &me) || free != 0 || total != 0 {
		t.Errorf("= %d, %d, %v; want *disk.MissingError and no numbers", free, total, err)
	}
}

func TestDockerDiskCheckStillRuns(t *testing.T) {
	d := newDiskTest(t)
	row := d.addDisk(t)
	f := readOf(row, 50*gig, 100*gig) // the node disk has room
	f.DiskFree, f.DiskTotal = gig/2, 10*gig
	d.facts(t, f)
	_, err := d.orch.diskCheck(context.Background(), "w1")
	if err == nil || !strings.Contains(err.Error(), "Docker disk") {
		t.Errorf("err = %v, want the Docker disk refusal", err)
	}
}

func TestEnvBannerOnlyForEnvsWithDiskVolumes(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	if _, ok := d.orch.EnvDiskDown(ctx, d.edge.ID); ok {
		t.Fatal("banner on a ready disk")
	}
	setState(t, d, row, disk.StateMissing)
	if _, ok := d.orch.EnvDiskDown(ctx, d.edge.ID); ok {
		t.Error("banner for an env with no disk volume")
	}
	setState(t, d, row, disk.StateReady)
	d.ensure(t, d.vol(t, "data"))
	setState(t, d, row, disk.StateMissing)
	if name, ok := d.orch.EnvDiskDown(ctx, d.edge.ID); !ok || name != "w1" {
		t.Errorf("= %q, %v, want w1", name, ok)
	}
	setState(t, d, row, disk.StateReady)
	if _, ok := d.orch.EnvDiskDown(ctx, d.edge.ID); ok {
		t.Error("banner once ready")
	}
}

func TestFactsPollReconcilesTheEchoedRegistryOnEverySuccessAndRepushesOnMismatch(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	workerNodeWith(t, o, "w1", node.StatusOnline, worker.Config{})
	put := &putMesh{}
	o.diskNodeOf = func(store.Node) (MeshNode, bool) { return put, true }

	must(t, o.refreshNodeClients(ctx))
	if put.pushes() != 0 {
		t.Fatalf("pushed with no disk and nothing echoed: %d", put.pushes())
	}
	must(t, o.store.NodeDisks.Create(ctx, store.NodeDisk{ID: "d1", NodeID: "w1", Kind: "path", MountPath: "/disk", State: disk.StateReady}))
	// the stub never installs it, so the echo stays empty: every read pushes
	must(t, o.refreshNodeClients(ctx))
	must(t, o.refreshNodeClients(ctx))
	if put.pushes() != 2 || put.last()[0].ID != "d1" {
		t.Errorf("pushes = %d, last %+v; want 2 of d1", put.pushes(), put.last())
	}
	// a real worker takes the push, echoes it, and is left alone after
	o.diskNodeOf = nil
	must(t, o.refreshNodeClients(ctx))
	must(t, o.refreshNodeClients(ctx))
	n, err := o.store.Nodes.Get(ctx, "w1")
	must(t, err)
	if f := o.reportedFacts(ctx, n); f == nil || len(f.DiskRegs) != 1 || f.DiskRegs[0].ID != "d1" {
		t.Fatalf("echoed registry = %+v", f)
	}
	o.diskNodeOf = func(store.Node) (MeshNode, bool) { return put, true }
	before := put.pushes()
	must(t, o.refreshNodeClients(ctx))
	if put.pushes() != before {
		t.Error("pushed although the worker echoes the registry")
	}
}
