package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// Move to disk (work package 2M). The rig: a plain volume "data" of the edge
// env on w1, a tile that mounts it, then a ready disk on w1.

type moveRig struct {
	*diskTest
	row  store.NodeDisk
	v    store.Volume
	tl   Tile
	gate chan struct{} // when set, the copy waits for it to be closed
}

func diskFacts(free uint64) worker.Facts {
	return worker.Facts{DisksRead: true, Disks: []hostdisk.Fact{{Path: "/disk", Mounted: true, Free: free, Total: 1 << 40}}}
}

func newMoveRig(t *testing.T) *moveRig {
	t.Helper()
	d := newDiskTest(t)
	v := d.vol(t, "data")
	d.ensure(t, v) // a plain volume: no disk yet
	r := &moveRig{diskTest: d, v: v}
	r.row = d.addDisk(t)
	r.tl = d.diskTile(t, "db", "data:/data")
	d.wFake.Volumes = []docker.VolumeInfo{{Name: v.Name, SizeBytes: 100}}
	d.wFake.Containers = []docker.Container{{ID: "c-db", Name: "db", State: "running",
		Labels: map[string]string{tile.LabelTile: r.tl.ID, tile.LabelRole: "replica"}}}
	d.facts(t, diskFacts(1<<30))
	// what an untar of a volume with data leaves in the folder
	d.wFake.RunID = "n-db" // a deploy makes a container with a new id
	d.wFake.Details = map[string]docker.Detail{"n-db": {Running: true, Networks: map[string]string{d.edge.Network: "10.0.0.2"}}}
	d.wFake.OnUntar = func(name string) {
		if r.gate != nil {
			<-r.gate
		}
		_ = os.WriteFile(filepath.Join(d.folder(r.row, name), "file"), []byte("data"), 0o644)
	}
	return r
}

// blockCopy makes the next copy wait, so the job stays running; the func lets it go.
func (r *moveRig) blockCopy() func() {
	r.gate = make(chan struct{})
	var once sync.Once
	return func() { once.Do(func() { close(r.gate) }) }
}

// volume is a second plain volume of the edge env mounted by its own tile.
func (r *moveRig) volume(t *testing.T, slug string) store.Volume {
	t.Helper()
	v := r.vol(t, slug) // a plain object with no folder row is a legacy volume
	r.wFake.Volumes = append(r.wFake.Volumes, docker.VolumeInfo{Name: v.Name, SizeBytes: 50})
	return v
}

func (r *moveRig) get(t *testing.T, id string) store.Volume {
	t.Helper()
	v, err := r.orch.volumes.Get(context.Background(), id)
	must(t, err)
	return v
}

func (r *moveRig) orphans(t *testing.T) []store.Volume {
	t.Helper()
	vs, err := r.st.Volumes.ListOrphaned(context.Background())
	must(t, err)
	return vs
}

func (r *moveRig) orphanNamed(t *testing.T, name string) (store.Volume, bool) {
	t.Helper()
	for _, o := range r.orphans(t) {
		if o.Name == name {
			return o, true
		}
	}
	return store.Volume{}, false
}

func (r *moveRig) move(t *testing.T, vs ...store.Volume) Job {
	t.Helper()
	j, err := r.orch.enqueueVolumeMove(context.Background(), adminP(), "w1", vs)
	return r.run(t, j, err)
}

// mover is a run of the group core made by hand, so a test can stop it after a
// step as a crash would.
func (r *moveRig) mover(t *testing.T, vs ...store.Volume) *volumeMove {
	t.Helper()
	p := volumeMoveJob{NodeID: "w1", By: "u-admin"}
	for _, v := range vs {
		p.Volumes = append(p.Volumes, volumeMoveItem{VolumeID: v.ID, Target: moveTarget(v.ID), OldName: v.Name})
	}
	m := &volumeMove{o: r.orch, p: p, r: &jobs.Run{Log: &bytes.Buffer{}}}
	must(t, m.load(context.Background(), true))
	m.r.Job = store.Job{ID: "j", LockSet: m.lock(), CreatedAt: time.Now()}
	return m
}

var errCrash = errors.New("crash")

// stopAfter ends the run after the named step, as a crash would.
func stopAfter(step string) func(string) error {
	return func(s string) error {
		if s == step {
			return errCrash
		}
		return nil
	}
}

func (r *moveRig) crash(t *testing.T, step string, vs ...store.Volume) {
	t.Helper()
	m := r.mover(t, vs...)
	m.after = stopAfter(step)
	if err := m.run(context.Background()); !errors.Is(err, errCrash) {
		t.Fatalf("run stopped after %q with %v, want the crash", step, err)
	}
}

func (r *moveRig) finish(t *testing.T, vs ...store.Volume) {
	t.Helper()
	if err := r.mover(t, vs...).run(context.Background()); err != nil {
		t.Fatalf("second click: %v", err)
	}
}

func (r *moveRig) wantMoved(t *testing.T, v store.Volume) {
	t.Helper()
	target := moveTarget(v.ID)
	if got := r.get(t, v.ID); got.Name != target {
		t.Errorf("row names %q, want %q", got.Name, target)
	}
	if o, ok := r.orphanNamed(t, v.Name); !ok || o.OrphanedAt == nil || o.NodeID != "w1" {
		t.Errorf("no orphan row for the old name %q: %+v", v.Name, o)
	}
	r.bound(t, r.wFake, r.row, target)
}

func (r *moveRig) wantUntouched(t *testing.T, v store.Volume) {
	t.Helper()
	if got := r.get(t, v.ID); got.Name != v.Name {
		t.Errorf("row names %q, want the old %q", got.Name, v.Name)
	}
	if len(r.orphans(t)) != 0 {
		t.Errorf("orphans: %+v", r.orphans(t))
	}
	if sawCall(r.wFake, "RemoveVolume("+v.Name+")") {
		t.Errorf("the old volume was removed: %v", callNames(r.wFake))
	}
}

// firstCall is the index of the first call with the prefix, -1 for none.
func (r *moveRig) firstCall(prefix string) int {
	return slices.IndexFunc(callNames(r.wFake), func(c string) bool { return strings.HasPrefix(c, prefix) })
}

// DL: the copy lands on the disk, the row's name becomes the target, the old
// name goes onto an orphaned row, and the tile deploys inline.
func TestMoveCopiesThenSwapsNameAndPutsTheOldNameOnAnOrphanRowAndDeploysInline(t *testing.T) {
	ctx := context.Background()
	r := newMoveRig(t)
	j, err := r.orch.MoveVolumeToDisk(ctx, adminP(), r.v.ID)
	if j = r.run(t, j, err); j.State != job.Done {
		t.Fatalf("move: %s %q", j.State, j.Error)
	}
	r.wantMoved(t, r.v)
	target := moveTarget(r.v.ID)
	if b, err := os.ReadFile(filepath.Join(r.folder(r.row, target), "file")); err != nil || string(b) != "data" {
		t.Errorf("the folder has no data: %q %v", b, err)
	}
	if sawCall(r.wFake, "RemoveVolume("+r.v.Name+")") {
		t.Errorf("the old copy went at once, not through the orphan job: %v", callNames(r.wFake))
	}
	// containers first, then the copy, then the deploy
	rm, un, dep := r.firstCall("StopRemove(c-db"), r.firstCall("UntarVolume("+target), r.firstCall("Create(")
	if rm < 0 || un < rm || dep < un {
		t.Errorf("order: remove %d, untar %d, deploy %d; calls %v", rm, un, dep, callNames(r.wFake))
	}
	evs, err := r.orch.SecurityLog(ctx, SecurityFilter{})
	must(t, err)
	if !slices.ContainsFunc(evs, func(e SecurityEvent) bool {
		return e.Action == "volume.to_disk" && e.TargetID == r.v.ID && e.ActorID == "u-admin"
	}) {
		t.Errorf("no volume.to_disk in the security log: %+v", evs)
	}
	// the orphan job removes the old copy later
	if o, ok := r.orphanNamed(t, r.v.Name); !ok {
		t.Fatal("no orphan")
	} else if o.InstanceID != nil || o.ScopeID != r.edge.ID {
		t.Errorf("orphan row = %+v", o)
	}
}

// DL: a copy that fails (here an empty one) leaves the old volume and the
// row, removes the new volume and its folder, and brings the tile back up.
func TestMoveFailureKeepsOldVolumeAndRow(t *testing.T) {
	r := newMoveRig(t)
	r.wFake.OnUntar = nil // the copy lands empty while the old volume holds data
	j := r.move(t, r.v)
	if j.State != job.Failed || !strings.Contains(j.Error, "empty") {
		t.Fatalf("move: %s %q, want failed with the empty copy", j.State, j.Error)
	}
	r.wantUntouched(t, r.v)
	target := moveTarget(r.v.ID)
	if !sawCall(r.wFake, "RemoveVolume("+target+")") {
		t.Errorf("the new volume stays: %v", callNames(r.wFake))
	}
	if _, err := os.Stat(r.folder(r.row, target)); err == nil {
		t.Error("the new folder stays")
	}
	if !sawCall(r.wFake, "Create(") {
		t.Errorf("the tile was not brought back up: %v", callNames(r.wFake))
	}
}

func TestMoveFailureOfTheCopyItselfKeepsOldVolumeAndRow(t *testing.T) {
	r := newMoveRig(t)
	r.wFake.Err = map[string]error{"UntarVolume": errors.New("disk write failed")}
	j := r.move(t, r.v)
	if j.State != job.Failed || !strings.Contains(j.Error, "disk write failed") {
		t.Fatalf("move: %s %q", j.State, j.Error)
	}
	r.wantUntouched(t, r.v)
}

func TestMoveRefusedForAVolumeOnAnotherDiskBackedName(t *testing.T) {
	r := newMoveRig(t)
	v := r.vol(t, "other")
	r.ensure(t, v) // a ready disk: a folder row and a bound object
	_, err := r.orch.MoveVolumeToDisk(context.Background(), adminP(), v.ID)
	if _, ok := errors.AsType[errs.Refused](err); !ok || !strings.Contains(err.Error(), "already on the disk") {
		t.Errorf("err = %v, want refused: already on the disk", err)
	}
}

func TestMoveTargetNameIsDerivedFromTheVolumeID(t *testing.T) {
	r := newMoveRig(t)
	if got, want := moveTarget(r.v.ID), "stackr-vol-"+r.v.ID+"-disk"; got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
	j := r.move(t, r.v)
	if j.State != job.Done {
		t.Fatalf("move: %s %q", j.State, j.Error)
	}
	if !strings.Contains(j.Payload, `"target":"`+moveTarget(r.v.ID)+`"`) {
		t.Errorf("payload = %s", j.Payload)
	}
	if got := r.get(t, r.v.ID).Name; got != moveTarget(r.v.ID) {
		t.Errorf("name = %q", got)
	}
}

func TestMoveRefusedWithoutRoom(t *testing.T) {
	r := newMoveRig(t)
	r.wFake.Volumes[0].SizeBytes = 1 << 30
	r.facts(t, diskFacts(1<<30+1<<20)) // a GB and a tenth over need more
	_, err := r.orch.MoveVolumeToDisk(context.Background(), adminP(), r.v.ID)
	if err == nil || !strings.Contains(err.Error(), "free") {
		t.Fatalf("err = %v, want refused for room", err)
	}
	r.facts(t, diskFacts(2<<30))
	if j := r.move(t, r.v); j.State != job.Done {
		t.Errorf("with room: %s %q", j.State, j.Error)
	}
}

func (r *moveRig) holder(t *testing.T, kind string, lock ...string) {
	t.Helper()
	must(t, r.st.Jobs.Create(context.Background(), store.Job{
		ID: uuid.NewString(), Kind: kind, State: job.Running, LockSet: store.StringList(lock),
		Payload: "{}", CreatedAt: time.Now(),
	}))
}

func TestMoveRefusedWhileBackupHoldsVolume(t *testing.T) {
	r := newMoveRig(t)
	r.holder(t, string(kindBackup), "volume:"+r.v.ID)
	_, err := r.orch.MoveVolumeToDisk(context.Background(), adminP(), r.v.ID)
	if err == nil || !strings.Contains(err.Error(), "holds the data volume") {
		t.Errorf("err = %v, want refused while the backup holds the volume", err)
	}
}

func TestMoveLockSetHasTheVolumeTheMountingTilesAndTheEnvConfigLockNotTheNode(t *testing.T) {
	r := newMoveRig(t)
	j, err := r.orch.MoveVolumeToDisk(context.Background(), adminP(), r.v.ID)
	must(t, err)
	r.wait(t, j)
	for _, want := range []string{"volume:" + r.v.ID, r.tl.ID, "env:" + r.edge.ID, "todisk:w1"} {
		if !slices.Contains(j.LockSet, want) {
			t.Errorf("lock set %v lacks %q", j.LockSet, want)
		}
	}
	for _, bad := range []string{"node:w1", r.edgeT.ID} {
		if slices.Contains(j.LockSet, bad) {
			t.Errorf("lock set %v holds %q", j.LockSet, bad)
		}
	}
}

// DL: the tiles that mount the volume are read again once the locks are held.
func TestMoveRefusedWhenTheMountingTilesChangedAfterTheLocksWereTaken(t *testing.T) {
	ctx := context.Background()
	r := newMoveRig(t)
	m := r.mover(t, r.v) // the lock set is taken here
	other := r.diskTile(t, "late", "data:/late")
	err := m.run(ctx)
	if _, ok := errors.AsType[errs.Conflict](err); !ok || !strings.Contains(err.Error(), "mounts changed") {
		t.Fatalf("err = %v, want the mounts changed refusal", err)
	}
	_ = other
	r.wantUntouched(t, r.v)
	if sawCall(r.wFake, "UntarVolume(") || sawCall(r.wFake, "StopRemove(") {
		t.Errorf("work began before the refusal: %v", callNames(r.wFake))
	}
}

// Other tiles on the node keep deploying while a move is in the middle of
// its copy: the move never takes the node's lock.
func TestMoveDoesNotBlockOtherTilesOnTheNode(t *testing.T) {
	ctx := context.Background()
	r := newMoveRig(t)
	release := r.blockCopy()
	j, err := r.orch.MoveVolumeToDisk(ctx, adminP(), r.v.ID)
	must(t, err)
	for range 300 { // wait for the copy to start
		if sawCall(r.wFake, "EnsureTool") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.wFake.RunID = "n-edge"
	r.wFake.Details["n-edge"] = docker.Detail{Running: true, Networks: map[string]string{r.edge.Network: "10.0.0.3"}}
	dj, err := r.orch.Deploy(ctx, r.edgeT.ID)
	if dj = r.run(t, dj, err); dj.State != job.Done {
		t.Fatalf("deploy of another tile: %s %q", dj.State, dj.Error)
	}
	if got, _ := r.orch.GetJob(ctx, j.ID); got.State != job.Running {
		t.Errorf("the move is %s, want still copying", got.State)
	}
	release()
	if j = r.wait(t, j); j.State != job.Done {
		t.Errorf("move: %s %q", j.State, j.Error)
	}
}

// DL: step 10 deploys inline: the job finishes, and no deploy job was queued
// behind the tile lock the move holds.
func TestMoveStep10RunsTheDeployFlowInlineAndDoesNotWaitOnItsOwnLock(t *testing.T) {
	r := newMoveRig(t)
	j := r.move(t, r.v)
	if j.State != job.Done {
		t.Fatalf("move: %s %q", j.State, j.Error)
	}
	last, ok, err := r.orch.jobRows.Last(context.Background(), r.tl.ID)
	must(t, err)
	if !ok || last.Kind != string(kindVolumeToDisk) {
		t.Errorf("the tile's last job is %q, want the move (no queued deploy)", last.Kind)
	}
	if !sawCall(r.wFake, "Create(") {
		t.Errorf("the tile did not deploy: %v", callNames(r.wFake))
	}
}

// DL: a second press never cancels or doubles a live move.
func TestMoveSecondPressRefusedWhileAVolumeToDiskJobIsLive(t *testing.T) {
	ctx := context.Background()
	r := newMoveRig(t)
	release := r.blockCopy()
	first, err := r.orch.MoveVolumeToDisk(ctx, adminP(), r.v.ID)
	must(t, err)
	_, err = r.orch.MoveVolumeToDisk(ctx, adminP(), r.v.ID)
	if _, ok := errors.AsType[errs.Conflict](err); !ok || !strings.Contains(err.Error(), "already moving") {
		t.Fatalf("second press = %v, want refused", err)
	}
	if got, _ := r.orch.GetJob(ctx, first.ID); got.State != job.Running && got.State != job.Queued {
		t.Errorf("first job is %s after the second press", got.State)
	}
	release()
	if first = r.wait(t, first); first.State != job.Done {
		t.Errorf("first job: %s %q", first.State, first.Error)
	}
}

func TestMoveFailureCleanupRunsOnAContextThatIsNotCancelled(t *testing.T) {
	r := newMoveRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	m := r.mover(t, r.v)
	m.after = func(step string) error {
		if step == "ensure" {
			cancel() // the job is cancelled mid-copy; the copy then fails
		}
		return nil
	}
	r.wFake.Err = map[string]error{"UntarVolume": context.Canceled}
	if err := m.run(ctx); err == nil {
		t.Fatal("run succeeded")
	}
	r.wantUntouched(t, r.v)
	if !sawCall(r.wFake, "RemoveVolume("+moveTarget(r.v.ID)+")") {
		t.Errorf("the new volume was not removed on a cancelled job: %v", callNames(r.wFake))
	}
	if !sawCall(r.wFake, "Create(") {
		t.Errorf("the tile was not brought back: %v", callNames(r.wFake))
	}
}

func TestMovePayloadRecordsTheActorAndTheOldName(t *testing.T) {
	r := newMoveRig(t)
	j, err := r.orch.MoveVolumeToDisk(context.Background(), adminP(), r.v.ID)
	must(t, err)
	r.wait(t, j)
	for _, want := range []string{`"by":"u-admin"`, `"old_name":"` + r.v.Name + `"`, `"volume_id":"` + r.v.ID + `"`, `"node_id":"w1"`} {
		if !strings.Contains(j.Payload, want) {
			t.Errorf("payload %s lacks %s", j.Payload, want)
		}
	}
}

// crash and resume

// DL: the row names the target and the disk has its folder row: the second
// click skips the copy and deploys. The branch comes before the "already on
// the disk" refusal, and nothing is untarred over the live new volume.
func TestMoveSecondClickWhenRowNameIsTheTargetAndAFolderRowExistsSkipsTheCopyAndDeploys(t *testing.T) {
	r := newMoveRig(t)
	r.crash(t, "swap", r.v)
	untars := slices.IndexFunc(callNames(r.wFake), func(c string) bool { return strings.HasPrefix(c, "UntarVolume(") })
	if untars < 0 {
		t.Fatal("the first run never copied")
	}
	before := len(callNames(r.wFake))
	r.wFake.Containers = nil
	r.finish(t, r.v)
	for _, c := range callNames(r.wFake)[before:] {
		if strings.HasPrefix(c, "UntarVolume(") || strings.HasPrefix(c, "TarVolume(") || strings.HasPrefix(c, "RemoveVolume(") {
			t.Errorf("the second click %s", c)
		}
	}
	if !sawCall(r.wFake, "Create(") {
		t.Error("the second click did not deploy")
	}
	r.wantMoved(t, r.v)
	if got := len(r.orphans(t)); got != 1 {
		t.Errorf("%d orphan rows, want 1", got)
	}
}

// DL
func TestMoveResumeRefusedWhenNoFolderRowExistsOnThisNodesDisk(t *testing.T) {
	r := newMoveRig(t)
	v := r.get(t, r.v.ID)
	v.Name = moveTarget(v.ID) // named for the disk, but the disk has nothing for it
	must(t, r.st.Volumes.Update(context.Background(), v))
	_, err := r.orch.MoveVolumeToDisk(context.Background(), adminP(), v.ID)
	if err == nil || !strings.Contains(err.Error(), "no folder") {
		t.Errorf("err = %v, want refused for the missing folder row", err)
	}
	if sawCall(r.wFake, "UntarVolume(") {
		t.Error("a copy ran")
	}
}

// DL
func TestMoveRefusedWhenAnotherRowOrMemberNamesTheTarget(t *testing.T) {
	ctx := context.Background()
	r := newMoveRig(t)
	clash := r.vol(t, "clash")
	clash.Name = moveTarget(r.v.ID)
	must(t, r.st.Volumes.Update(ctx, clash))
	if _, err := r.orch.MoveVolumeToDisk(ctx, adminP(), r.v.ID); err == nil || !strings.Contains(err.Error(), "already uses the name") {
		t.Errorf("another row: err = %v", err)
	}
	clash.Name = "stackr-vol-" + clash.ID
	must(t, r.st.Volumes.Update(ctx, clash))
	in, err := r.orch.operated.Create(ctx, r.devT.ID, "postgres", "stackr", "")
	must(t, err)
	must(t, r.st.OperatedMembers.Create(ctx, store.OperatedMember{
		ID: uuid.NewString(), InstanceID: in.ID, Role: "replica", NodeID: "w1", Volume: moveTarget(r.v.ID),
		State: "running", CreatedAt: time.Now(),
	}))
	if _, err := r.orch.MoveVolumeToDisk(ctx, adminP(), r.v.ID); err == nil || !strings.Contains(err.Error(), "standby already uses") {
		t.Errorf("a member: err = %v", err)
	}
}

// DL: after EnsureNamed the volume must be bound to its folder, never a plain
// one made behind the disk's back.
func TestMoveAfterEnsureRequiresRouteDisk(t *testing.T) {
	r := newMoveRig(t)
	// a daemon that keeps listing the plain target after its remove makes the
	// next make a plain one: the move must see it is not on the disk
	r.wFake.Volumes = append(r.wFake.Volumes, docker.VolumeInfo{Name: moveTarget(r.v.ID)})
	j := r.move(t, r.v)
	if j.State != job.Failed || !strings.Contains(j.Error, "did not come up on the disk") {
		t.Fatalf("move: %s %q, want failed: not on the disk", j.State, j.Error)
	}
	r.wantUntouched(t, r.v)
	if sawCall(r.wFake, "UntarVolume(") {
		t.Error("a copy ran into a plain volume")
	}
}

// DL: a ghost of the target (a plain copy beside a folder row) is the
// admin's call: the move refuses and the old volume stays the live one.
func TestMoveGhostOfTheTargetIsRefusedAndKeepsTheOldVolume(t *testing.T) {
	r := newMoveRig(t)
	target := moveTarget(r.v.ID)
	_, err := r.orch.diskLeaf.Place(context.Background(), r.row, target)
	must(t, err)
	r.wFake.Volumes = append(r.wFake.Volumes, docker.VolumeInfo{Name: target, SizeBytes: 5})
	j := r.move(t, r.v)
	if j.State != job.Failed || !strings.Contains(j.Error, "outside the disk") {
		t.Fatalf("move: %s %q, want failed on the ghost", j.State, j.Error)
	}
	r.wantUntouched(t, r.v)
	if sawCall(r.wFake, "UntarVolume(") {
		t.Error("a copy ran over the ghost")
	}
}

// DL: a second click after a crash anywhere before the swap never untars
// over a live new volume: the row still names the old one, so it is a fresh
// move that discards the partial target first.
func TestMoveSecondClickNeverUntarsOverTheLiveNewVolume(t *testing.T) {
	r := newMoveRig(t)
	r.crash(t, "swap", r.v) // swap is the last step; the row is committed
	before := len(callNames(r.wFake))
	r.finish(t, r.v)
	for _, c := range callNames(r.wFake)[before:] {
		if strings.HasPrefix(c, "UntarVolume(") {
			t.Errorf("untar over the live new volume: %s", c)
		}
	}
	if got := r.get(t, r.v.ID).Name; got != moveTarget(r.v.ID) {
		t.Errorf("name = %q", got)
	}
}

// DL
func TestMoveCrashAfterContainerRemovalResumes(t *testing.T) {
	r := newMoveRig(t)
	r.crash(t, "containers", r.v)
	r.wantUntouched(t, r.v)
	r.wFake.Containers = nil // they are gone
	r.finish(t, r.v)
	r.wantMoved(t, r.v)
}

// DL
func TestMoveCrashAfterAPartialCopyDiscardsItAndCopiesAgain(t *testing.T) {
	r := newMoveRig(t)
	target := moveTarget(r.v.ID)
	r.crash(t, "copy", r.v)
	r.wantUntouched(t, r.v)
	// the partial copy: a stray file only a partial untar would leave
	must(t, os.WriteFile(filepath.Join(r.folder(r.row, target), "partial"), []byte("x"), 0o644))
	before := len(callNames(r.wFake))
	r.finish(t, r.v)
	r.wantMoved(t, r.v)
	if _, err := os.Stat(filepath.Join(r.folder(r.row, target), "partial")); err == nil {
		t.Error("the partial copy stayed")
	}
	rest := callNames(r.wFake)[before:]
	rm := slices.IndexFunc(rest, func(c string) bool { return c == "RemoveVolume("+target+")" })
	un := slices.IndexFunc(rest, func(c string) bool { return strings.HasPrefix(c, "UntarVolume("+target) })
	if rm < 0 || un < rm {
		t.Errorf("the target was not discarded before the copy: %v", rest)
	}
}

// DL
func TestMoveCrashAfterTheSwapResumesAtTheDeploy(t *testing.T) {
	r := newMoveRig(t)
	r.crash(t, "swap", r.v)
	r.wantMoved(t, r.v)
	if sawCall(r.wFake, "Create(") {
		t.Fatal("the deploy ran before the crash")
	}
	r.wFake.Containers = nil
	r.finish(t, r.v)
	if !sawCall(r.wFake, "Create(") {
		t.Error("the resume did not deploy")
	}
	r.wantMoved(t, r.v)
}

// DL: after the deploy the old copy is left to the orphan job, whatever the
// deploy did.
func TestMoveCrashAfterTheDeployLeavesTheOldCopyToTheOrphanJob(t *testing.T) {
	r := newMoveRig(t)
	r.crash(t, "deploy", r.v)
	r.wantMoved(t, r.v)
	if sawCall(r.wFake, "RemoveVolume("+r.v.Name+")") {
		t.Errorf("the old copy was removed by the move: %v", callNames(r.wFake))
	}
	// the orphan job removes it once retention has passed
	if log := r.orphans(t); len(log) != 1 {
		t.Errorf("orphans = %+v", log)
	}
}

// A group of two: the second volume fails, so the first (copied and checked)
// is swapped, the second keeps its old copy, the tiles deploy once on that
// mixed set and the old copy of the first goes only through its orphan row.
func TestMoveGroupFailureOnTheSecondDeploysTheMixedSetOnce(t *testing.T) {
	ctx := context.Background()
	r := newMoveRig(t)
	v2 := r.volume(t, "logs")
	app := r.diskTile(t, "app", "data:/data\nlogs:/logs") // links the two volumes into one group
	r.wFake.Containers = append(r.wFake.Containers, docker.Container{ID: "c-app", Name: "app", State: "running",
		Labels: map[string]string{tile.LabelTile: app.ID, tile.LabelRole: "replica"}})
	r.wFake.Err = map[string]error{"UntarVolume(" + moveTarget(v2.ID) + ")": errors.New("disk write failed")}
	m := r.mover(t, r.v, v2)
	err := m.run(ctx)
	if err == nil || !strings.Contains(err.Error(), "disk write failed") {
		t.Fatalf("run = %v", err)
	}
	r.wantMoved(t, r.v)
	if got := r.get(t, v2.ID); got.Name != v2.Name {
		t.Errorf("the failed volume now names %q", got.Name)
	}
	if o, ok := r.orphanNamed(t, v2.Name); ok {
		t.Errorf("the failed volume has an orphan row: %+v", o)
	}
	// the tiles went down once, before the first copy, and nothing stops them
	// again until the one deploy of the mixed set
	calls := callNames(r.wFake)
	first := func(prefix string) int {
		return slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
	}
	untar, deploy := first("UntarVolume("), first("Create(")
	for _, c := range []string{"StopRemove(c-db", "StopRemove(c-app"} {
		if i := first(c); i < 0 || i > untar {
			t.Errorf("%s) at %d, want before the first copy at %d", c, i, untar)
		}
	}
	for i := untar; i < deploy; i++ {
		if strings.HasPrefix(calls[i], "StopRemove(") {
			t.Errorf("a tile was stopped a second time: %s", calls[i])
		}
	}
	for _, c := range []string{"Create(stackr-db", "Create(stackr-app"} {
		if n := countCalls(r.wFake, c); n != 1 {
			t.Errorf("%s ran %d times, want one deploy", c, n)
		}
	}
	if sawCall(r.wFake, "RemoveVolume("+r.v.Name+")") {
		t.Errorf("an old copy was removed outside the orphan job: %v", callNames(r.wFake))
	}
}
