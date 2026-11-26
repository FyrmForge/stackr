package service

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/upgrade"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// Move logs and metrics onto the disk (RFC 72bb, section 8). The rig is the
// disk rig with the node disk on the controller and a plain observe volume
// (100 bytes) on it.

type observeRig struct {
	*diskTest
	row   store.NodeDisk
	steps []string
	mu    sync.Mutex
	check func(context.Context) error
}

func (r *observeRig) ctl() string { return r.diskTest.ctl.ID }

func newObserveRig(t *testing.T) *observeRig {
	t.Helper()
	ctx := context.Background()
	d := newDiskTest(t)
	r := &observeRig{diskTest: d}
	r.facts(t, worker.Facts{})
	r.ctlFacts(t, worker.Facts{DisksRead: true})
	d.mount(t, "/disk")
	j, err := d.orch.AddNodeFolder(ctx, adminP(), r.ctl(), "/disk")
	if j = d.run(t, j, err); j.State != job.Done {
		t.Fatalf("add disk: %s %s", j.State, j.Error)
	}
	var ok bool
	if r.row, ok, err = d.orch.nodeDisk(ctx, r.ctl()); err != nil || !ok {
		t.Fatalf("disk row: %v %v", ok, err)
	}
	r.ctlFacts(t, ctlDiskFacts(1<<30))
	d.ctlFake.Volumes = []docker.VolumeInfo{{Name: installspec.ObserveVolume, SizeBytes: 100}}
	d.ctlFake.OnUntar = func(name string) {
		_ = os.WriteFile(filepath.Join(d.folder(r.row, name), "file"), []byte("data"), 0o644)
	}
	d.orch.observe.moveStep = func(s string) {
		r.mu.Lock()
		r.steps = append(r.steps, s)
		r.mu.Unlock()
	}
	d.orch.observe.check = func(ctx context.Context) error {
		if r.check != nil {
			return r.check(ctx)
		}
		return nil
	}
	must(t, d.orch.SetSettings(ctx, map[string]string{"observe_enabled": "true"}))
	return r
}

func ctlDiskFacts(free uint64) worker.Facts {
	f := diskFacts(free)
	f.DiskFree, f.DiskTotal = 1<<30, 1<<40
	return f
}

func (r *observeRig) ctlFacts(t *testing.T, f worker.Facts) {
	t.Helper()
	m := r.orch.mesh
	m.mu.Lock()
	m.facts[r.ctl()] = f
	m.at[r.ctl()] = time.Now()
	m.mu.Unlock()
}

func (r *observeRig) move(t *testing.T, back bool) Job {
	t.Helper()
	j, err := r.orch.MoveObserve(context.Background(), adminP(), r.ctl(), back)
	must(t, err)
	return r.wait(t, j)
}

func (r *observeRig) stepNames() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.steps, ",")
}

func (r *observeRig) onDisk() bool {
	on, err := r.orch.observeOnDisk(context.Background())
	if err != nil {
		panic(err)
	}
	return on
}

// removed is whether the controller's daemon was told to remove the volume.
func (r *observeRig) removed(name string) bool {
	return sawCall(r.ctlFake, "RemoveVolume("+name+")")
}

func TestObserveMoveToDiskAndBack(t *testing.T) {
	ctx := context.Background()
	r := newObserveRig(t)
	p, err := r.orch.PlanMoveObserve(ctx, adminP(), r.ctl(), false)
	must(t, err)
	if p.SizeBytes != 100 || p.Need != 110 || !p.Fits || p.Cap < 1<<30 || p.Busy {
		t.Fatalf("plan = %+v", p)
	}
	if j := r.move(t, false); j.State != job.Done {
		t.Fatalf("to disk: %s %s", j.State, j.Error)
	}
	if !r.onDisk() {
		t.Error("the setting did not flip")
	}
	if got := r.stepNames(); got != "stopped,ensure,copy,check,switched,done" {
		t.Errorf("steps = %s", got)
	}
	if st, err := os.Stat(filepath.Join(r.folder(r.row, installspec.ObserveDiskVolume), "file")); err != nil || st.IsDir() {
		t.Errorf("no copy on the disk: %v", err)
	}
	if ok, dev, _ := r.ctlFake.VolumeDevice(ctx, installspec.ObserveDiskVolume); !ok || dev == "" {
		t.Errorf("disk volume device = %q (found %v)", dev, ok)
	}
	if !r.removed(installspec.ObserveVolume) {
		t.Error("the old plain volume was not removed")
	}
	if v, _, _ := r.st.Settings.Get(ctx, observeDataCapKey); v == "" {
		t.Error("no xfs cap saved")
	} else if n, _ := strconv.ParseInt(v, 10, 64); n < 1<<30 {
		t.Errorf("cap = %s", v)
	}
	if c, err := r.orch.folderCap(ctx, installspec.ObserveDiskVolume); err != nil || c < 1<<30 {
		t.Errorf("folderCap = %d, %v", c, err)
	}
	if _, err := r.orch.PlanMoveObserve(ctx, adminP(), r.ctl(), false); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("a second move to the disk: %v", err)
	}

	// back: the plain volume again, the folder and its row gone
	r.steps = nil
	if j := r.move(t, true); j.State != job.Done {
		t.Fatalf("back: %s %s", j.State, j.Error)
	}
	if r.onDisk() {
		t.Error("the setting stayed on")
	}
	if has, err := r.orch.diskLeaf.HasFolders(ctx, r.row, installspec.ObserveDiskVolume); err != nil || has[installspec.ObserveDiskVolume] {
		t.Errorf("the disk folder row stayed: %v %v", has, err)
	}
	if !r.removed(installspec.ObserveDiskVolume) || !sawCall(r.ctlFake, "CreateVolume("+installspec.ObserveVolume+")") {
		t.Errorf("calls after back = %v", callNames(r.ctlFake))
	}
	if _, err := r.orch.PlanMoveObserve(ctx, adminP(), r.ctl(), true); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("a second move back: %v", err)
	}
}

// The job holds observe.mu for its whole run and calls the inner sync, so it
// does not deadlock on itself, a round that fails to reach the store opens no
// banner meanwhile, and the real wrapper works once the job is done.
func TestObserveMoveHoldsTheLockWithoutDeadlockOrBanner(t *testing.T) {
	ctx := context.Background()
	r := newObserveRig(t)
	o := r.orch
	unreachable := &net.OpError{Op: "dial", Err: errors.New("down")}
	var held bool
	o.observe.moveStep = func(step string) {
		if step != "copy" {
			return
		}
		held = !o.observe.mu.TryLock()
		for range 3 {
			o.observeReach(ctx, unreachable)
		}
	}
	if j := r.move(t, false); j.State != job.Done {
		t.Fatalf("job: %s %s", j.State, j.Error)
	}
	if !held {
		t.Error("observe.mu was free during the copy")
	}
	if o.hasProblem(ctx, observeDownKey) {
		t.Error("a banner opened during the move")
	}
	// the lock is released: the wrapper runs, and a failing store now opens the banner
	done := make(chan struct{})
	go func() { o.syncObserve(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("syncObserve blocked after the job")
	}
	for range 3 {
		o.observeReach(ctx, unreachable)
	}
	if !o.hasProblem(ctx, observeDownKey) {
		t.Error("no banner once the lock is free")
	}
}

// A retry wipes the destination: a failed run leaves its half copy, the next
// one starts clean. A failed proof puts the setting and observe back on the old
// volume, which is not removed.
func TestObserveMoveRetryWipesTheDestination(t *testing.T) {
	ctx := context.Background()
	r := newObserveRig(t)
	r.check = func(context.Context) error { return errors.New("sockets do not answer") }
	j := r.move(t, false)
	if j.State != job.Failed || !strings.Contains(j.Error, "sockets do not answer") {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	if r.onDisk() {
		t.Error("the setting stayed on after a failed proof")
	}
	if r.removed(installspec.ObserveVolume) {
		t.Error("the old volume went although the move failed")
	}
	stale := filepath.Join(r.folder(r.row, installspec.ObserveDiskVolume), "stale")
	must(t, os.WriteFile(stale, []byte("half"), 0o644))

	r.check = nil
	if j = r.move(t, false); j.State != job.Done {
		t.Fatalf("retry: %s %s", j.State, j.Error)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the half copy survived the retry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.folder(r.row, installspec.ObserveDiskVolume), "file")); err != nil {
		t.Errorf("no copy after the retry: %v", err)
	}
	if !r.onDisk() {
		t.Error("not on the disk after the retry")
	}
	_ = ctx
}

// The disk guard: no move onto or off a disk that is not ready, and observe
// stays down (with the banner naming the disk) while its data is on a missing
// disk.
func TestObserveMoveDiskMissingGuard(t *testing.T) {
	ctx := context.Background()
	r := newObserveRig(t)
	setState(t, r.diskTest, r.row, "missing")
	if _, err := r.orch.MoveObserve(ctx, adminP(), r.ctl(), false); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("move with a missing disk: %v", err)
	}
	must(t, r.st.Settings.Set(ctx, observeDataDiskKey, "1"))
	if _, err := r.orch.MoveObserve(ctx, adminP(), r.ctl(), true); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("move back with a missing disk: %v", err)
	}

	cctx, err := r.orch.withController(ctx)
	must(t, err)
	r.orch.syncObserve(cctx)
	if e := r.orch.observe.lastErr.Load(); e == nil || *e != upgrade.ObserveDiskMissing {
		t.Fatalf("lastErr = %v", e)
	}
	if got := r.orch.observeDownText(); !strings.Contains(got, "node disk is missing") {
		t.Errorf("banner = %q", got)
	}
	// the disk is back: observe may start again
	setState(t, r.diskTest, r.row, "ready")
	r.orch.syncObserve(cctx)
	if e := r.orch.observe.lastErr.Load(); e == nil || *e != "" {
		t.Errorf("lastErr after the disk came back = %v", e)
	}
}

func TestObserveMoveRefusals(t *testing.T) {
	ctx := context.Background()
	r := newObserveRig(t)
	// only the controller
	if _, err := r.orch.PlanMoveObserve(ctx, adminP(), "w1", false); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("on a worker: %v", err)
	}
	// not on the disk: nothing to move back
	if _, err := r.orch.MoveObserve(ctx, adminP(), r.ctl(), true); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("back while off the disk: %v", err)
	}
	// no room
	r.ctlFacts(t, ctlDiskFacts(50))
	if _, err := r.orch.MoveObserve(ctx, adminP(), r.ctl(), false); !errors.As(err, new(errs.Conflict)) {
		t.Errorf("without room: %v", err)
	}
	r.ctlFacts(t, ctlDiskFacts(1<<30))
	// a move that holds the node's todisk key
	j, err := r.orch.enqueue(ctx, kindObserveMove, observeMoveJob{NodeID: r.ctl()}, todiskKey(r.ctl()), "block")
	must(t, err)
	if p, err := r.orch.PlanMoveObserve(ctx, adminP(), r.ctl(), false); err != nil || !p.Busy {
		t.Errorf("busy plan = %+v, %v", p, err)
	}
	_ = j
	if _, err := r.orch.MoveObserve(ctx, adminP(), r.ctl(), false); !errors.As(err, new(errs.Conflict)) {
		t.Errorf("while another move holds the key: %v", err)
	}
}

// Forget (and Adopt) of the controller's disk ask this: refused while observe
// is on it, quiet otherwise and for any other node.
func TestObserveDiskRefusal(t *testing.T) {
	ctx := context.Background()
	r := newObserveRig(t)
	if err := r.orch.observeDiskRefusal(ctx, r.ctl()); err != nil {
		t.Fatalf("observe off the disk: %v", err)
	}
	must(t, r.st.Settings.Set(ctx, observeDataDiskKey, "1"))
	if err := r.orch.observeDiskRefusal(ctx, r.ctl()); !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "Move them back") {
		t.Errorf("observe on the disk: %v", err)
	}
	if err := r.orch.observeDiskRefusal(ctx, "w1"); err != nil {
		t.Errorf("another node: %v", err)
	}
}
