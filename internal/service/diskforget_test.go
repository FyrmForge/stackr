package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// forgetWorld is w1 with a disk that holds volume "data" (with a file in its
// folder), mounted by tile db, whose container runs. lose() makes the disk
// missing, the state a forget starts from.
type forgetWorld struct {
	*diskTest
	row  store.NodeDisk
	v    store.Volume
	tl   Tile
	file string
	cid  string
}

func newForgetWorld(t *testing.T, withTile, withBackup bool) *forgetWorld {
	t.Helper()
	d := newDiskTest(t)
	w := &forgetWorld{diskTest: d, row: d.addDisk(t), cid: "c-db"}
	w.v = d.vol(t, "data")
	d.ensure(t, w.v)
	w.file = filepath.Join(d.folder(w.row, w.v.Name), "DATA")
	must(t, os.WriteFile(w.file, []byte("precious"), 0o644))
	if withTile {
		w.tl = d.diskTile(t, "db", "data:/data")
		d.wFake.Containers = append(d.wFake.Containers, docker.Container{ID: w.cid, Name: "db", State: "running",
			Labels: map[string]string{tile.LabelTile: w.tl.ID, tile.LabelRole: "replica"}})
	}
	if withBackup {
		ctx := context.Background()
		must(t, d.st.BackupDests.Create(ctx, store.BackupDest{ID: "dest", Kind: "s3", Name: "dest", Endpoint: "e", Bucket: "b", AccessKey: "a", SecretKey: "s", CreatedAt: time.Now()}))
		vid := w.v.ID
		must(t, d.st.BackupRuns.Create(ctx, store.BackupRun{ID: "run1", Kind: "volume", VolumeID: &vid, DestID: "dest", Status: backup.Done, ObjectKey: "k.tar.age", CreatedAt: time.Now()}))
	}
	return w
}

func (w *forgetWorld) lose(t *testing.T) {
	t.Helper()
	setState(t, w.diskTest, w.row, disk.StateMissing)
}

func (w *forgetWorld) forget(t *testing.T) Job {
	t.Helper()
	j, err := w.orch.ForgetNodeDisk(context.Background(), adminP(), w.row.ID, "w1")
	return w.run(t, j, err)
}

func (w *forgetWorld) rowGone(t *testing.T) bool {
	t.Helper()
	_, err := w.st.NodeDisks.Get(context.Background(), w.row.ID)
	return errors.Is(err, errs.ErrNotFound)
}

func (w *forgetWorld) state(t *testing.T) string { return w.stateOf(t, w.row) }

func (w *forgetWorld) folders(t *testing.T) int {
	t.Helper()
	fs, err := w.st.DiskFolders.ListByDisk(context.Background(), w.row.ID)
	must(t, err)
	return len(fs)
}

func (w *forgetWorld) jobsOf(t *testing.T, kind string) []store.Job {
	t.Helper()
	rs, err := w.st.Jobs.ListByState(context.Background(), job.Queued, job.Running, job.Done, job.Failed, job.Waiting)
	must(t, err)
	return slices.DeleteFunc(rs, func(j store.Job) bool { return j.Kind != kind })
}

func jobLog(t *testing.T, d *diskTest, j Job) string {
	t.Helper()
	_, l, err := d.orch.PollJob(context.Background(), j.ID, 0)
	must(t, err)
	return string(l.Chunk)
}

func TestForgetRefusedForAReadyDisk(t *testing.T) {
	w := newForgetWorld(t, true, false)
	_, err := w.orch.ForgetNodeDisk(context.Background(), adminP(), w.row.ID, "w1")
	if !errors.Is(err, errs.ErrRefused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if w.state(t) != disk.StateReady || sawCall(w.wFake, "StopRemove(") || sawCall(w.wFake, "RemoveVolume(") {
		t.Errorf("something changed: %s %v", w.state(t), callNames(w.wFake))
	}
}

func TestForgetNeedsTheTypedConfirmAndAdmin(t *testing.T) {
	w := newForgetWorld(t, true, false)
	w.lose(t)
	if _, err := w.orch.ForgetNodeDisk(context.Background(), adminP(), w.row.ID, "nope"); !isInvalid(err) {
		t.Errorf("wrong confirm = %v", err)
	}
	if _, err := w.orch.ForgetNodeDisk(context.Background(), ownerP("x"), w.row.ID, "w1"); err == nil {
		t.Error("a non-admin forgot a disk")
	}
	if w.state(t) != disk.StateMissing {
		t.Errorf("state = %s", w.state(t))
	}
	// the disk id works as the confirm too
	j, err := w.orch.ForgetNodeDisk(context.Background(), adminP(), w.row.ID, w.row.ID)
	if j = w.run(t, j, err); j.State != job.Done {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
}

// DL: containers go before the bound objects (Docker refuses a volume a
// container still references), and the folder keeps its data.
func TestForgetRemovesContainersBeforeBoundObjects(t *testing.T) {
	w := newForgetWorld(t, true, false)
	w.lose(t)
	if j := w.forget(t); j.State != job.Done {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	c, v := indexOf(w.wFake, "StopRemove("+w.cid), indexOf(w.wFake, "RemoveVolume("+w.v.Name)
	if c < 0 || v < 0 || c > v {
		t.Errorf("order: StopRemove at %d, RemoveVolume at %d: %v", c, v, callNames(w.wFake))
	}
}

// DL: the bound objects go and the folder, its row and its data stay until
// the row is deleted, and the data stays on the host after it.
func TestForgetDropsBoundObjectsAndKeepsFolderContents(t *testing.T) {
	w := newForgetWorld(t, true, false)
	w.lose(t)
	if j := w.forget(t); j.State != job.Done {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	if !sawCall(w.wFake, "RemoveVolume("+w.v.Name+")") {
		t.Errorf("the object was not removed: %v", callNames(w.wFake))
	}
	if b, err := os.ReadFile(w.file); err != nil || string(b) != "precious" {
		t.Errorf("the folder's data = %q, %v", b, err)
	}
}

func TestForgetThenRestoreDeployRestoresFromBackup(t *testing.T) {
	w := newForgetWorld(t, true, true)
	w.lose(t)
	if j := w.forget(t); j.State != job.Done {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	if len(w.jobsOf(t, string(kindRestore))) != 1 || len(w.jobsOf(t, string(kindDeploy))) == 0 {
		t.Errorf("restore jobs %d, deploy jobs %d", len(w.jobsOf(t, string(kindRestore))), len(w.jobsOf(t, string(kindDeploy))))
	}
	// the restore target is a plain volume on the Docker disk
	var plain bool
	for _, c := range w.wFake.Created {
		plain = plain || c.Name == w.v.Name && c.Opts == nil
	}
	if !plain {
		t.Errorf("no plain volume made: %+v", w.wFake.Created)
	}
}

// DL: the row is deleted last, and only with it the folder rows. A failing
// restore step keeps the row forgetting with its folder rows; the next press
// finishes and the cascade removes them.
func TestForgetDeletesTheDiskRowLastAndCascadesTheFolderRows(t *testing.T) {
	w := newForgetWorld(t, true, true)
	w.lose(t)
	w.wFake.Err = map[string]error{"CreateVolume("+w.v.Name+")": errors.New("daemon busy")}
	if j := w.forget(t); j.State != job.Failed {
		t.Fatalf("job = %s, want failed", j.State)
	}
	if w.rowGone(t) || w.state(t) != disk.StateForgetting || w.folders(t) != 1 {
		t.Fatalf("after the failure: gone %v, state %s, folders %d", w.rowGone(t), w.state(t), w.folders(t))
	}
	w.wFake.Edit(func() { w.wFake.Err = nil })
	if j := w.forget(t); j.State != job.Done {
		t.Fatalf("second press = %s %q", j.State, j.Error)
	}
	if !w.rowGone(t) || w.folders(t) != 0 {
		t.Errorf("gone %v, folders %d", w.rowGone(t), w.folders(t))
	}
	evs, err := w.orch.SecurityLog(context.Background(), SecurityFilter{})
	must(t, err)
	if !slices.ContainsFunc(evs, func(e SecurityEvent) bool { return e.Action == "disk.forget" }) {
		t.Error("no disk.forget in the security log")
	}
}

// a disk row of a node that is not a node any more
func (d *diskTest) goneDisk(t *testing.T, state string) store.NodeDisk {
	t.Helper()
	ctx := context.Background()
	row := store.NodeDisk{ID: uuid.NewString(), NodeID: "gone", Kind: "path", MountPath: "/g", State: state, CreatedAt: time.Now()}
	must(t, d.st.NodeDisks.Create(ctx, row))
	must(t, d.st.DiskFolders.Create(ctx, store.DiskFolder{ID: uuid.NewString(), DiskID: row.ID, Name: "x", CreatedAt: time.Now()}))
	must(t, d.st.ForgottenNodes.Create(ctx, store.ForgottenNode{ID: "gone", Name: "h3", ForgottenAt: time.Now()}))
	return row
}

func TestForgetWithGoneNodeIsMetadataOnly(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.goneDisk(t, disk.StateReady) // even a ready row of a forgotten node goes
	calls := len(d.wFake.Calls())
	j, err := d.orch.ForgetNodeDisk(ctx, adminP(), row.ID, "h3")
	must(t, err)
	if j.ID != "" {
		t.Errorf("a job was queued: %+v", j)
	}
	if _, err := d.st.NodeDisks.Get(ctx, row.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("row = %v", err)
	}
	if fs, _ := d.st.DiskFolders.ListByDisk(ctx, row.ID); len(fs) != 0 {
		t.Errorf("folder rows left: %v", fs)
	}
	if len(d.wFake.Calls()) != calls {
		t.Errorf("Docker was asked: %v", callNames(d.wFake))
	}
	evs, err := d.orch.SecurityLog(ctx, SecurityFilter{})
	must(t, err)
	if !slices.ContainsFunc(evs, func(e SecurityEvent) bool { return e.Action == "disk.forget" }) {
		t.Error("no disk.forget in the security log")
	}
}

// DL: while an env still points at the forgotten node nothing is deleted.
func TestForgetWithGoneNodeRefusedWhileAnEnvPointsAtIt(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.goneDisk(t, disk.StateReady)
	must(t, d.st.Environments.SetNode(ctx, d.edge.ID, "gone"))
	_, err := d.orch.ForgetNodeDisk(ctx, adminP(), row.ID, row.ID)
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "1 envs still point at the forgotten node: recover them first") {
		t.Fatalf("err = %v", err)
	}
	if got, err := d.st.NodeDisks.Get(ctx, row.ID); err != nil || got.State != disk.StateReady {
		t.Errorf("row = %+v, %v", got, err)
	}
	if fs, _ := d.st.DiskFolders.ListByDisk(ctx, row.ID); len(fs) != 1 {
		t.Errorf("folder rows = %v", fs)
	}
}

func TestForgetRefusedForAnAdoptingRowAfterTheTransaction(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	row.State, row.OpNodeID, row.OpFromNodeID = disk.StateAdopting, "w1", "gone"
	must(t, d.st.NodeDisks.Update(ctx, row))
	for _, confirm := range []string{"w1", row.ID} {
		if _, err := d.orch.ForgetNodeDisk(ctx, adminP(), row.ID, confirm); !errors.Is(err, errs.ErrRefused) {
			t.Errorf("confirm %s: err = %v, want a refusal", confirm, err)
		}
	}
	if d.stateOf(t, row) != disk.StateAdopting || sawCall(d.wFake, "RemoveVolume(") {
		t.Errorf("state %s, calls %v", d.stateOf(t, row), callNames(d.wFake))
	}
}

// A volume of the env on the disk owned by an instance whose standby row is
// on node standbyNode.
func (w *forgetWorld) instanceStandby(t *testing.T, standbyNode string) {
	t.Helper()
	ctx := context.Background()
	m, err := w.orch.operated.Create(ctx, w.devT.ID, "postgres", "stackr", "")
	must(t, err)
	v := w.v
	v.InstanceID = &m.ID
	must(t, w.st.Volumes.Update(ctx, v))
	must(t, w.st.OperatedMembers.Create(ctx, store.OperatedMember{ID: "m-std", InstanceID: m.ID, Role: opmember.Replica,
		NodeID: standbyNode, Volume: "std-vol", State: opmember.Running, Phase: opmember.Streaming, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
}

// DL: a primary on the disk with a standby alive elsewhere: refused, with no
// row change and no container removed.
func TestForgetRefusedBeforeAnyChangeForAPrimaryOnTheDiskWithAStandbyAliveElsewhere(t *testing.T) {
	w := newForgetWorld(t, true, false)
	w.lose(t)
	w.instanceStandby(t, w.ctl.ID)
	_, err := w.orch.ForgetNodeDisk(context.Background(), adminP(), w.row.ID, "w1")
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "Promote or remove the standby first") {
		t.Fatalf("err = %v", err)
	}
	if w.state(t) != disk.StateMissing || sawCall(w.wFake, "StopRemove(") || sawCall(w.wFake, "RemoveVolume(") {
		t.Errorf("state %s, calls %v", w.state(t), callNames(w.wFake))
	}
}

// DL: every refusal, also the job's own re-run of it, comes before step 1.
func TestForgetRefusalChecksRunBeforeStep1(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, true, false)
	w.lose(t)
	w.instanceStandby(t, w.ctl.ID)
	// the verb refuses; a job enqueued past the verb refuses too
	j, err := w.orch.enqueue(ctx, kindDiskForget, diskForgetJob{DiskID: w.row.ID})
	must(t, err)
	if j = w.wait(t, j); j.State != job.Failed || !strings.Contains(j.Error, "Promote or remove the standby first") {
		t.Errorf("job = %s %q", j.State, j.Error)
	}
	if w.state(t) != disk.StateMissing || sawCall(w.wFake, "StopRemove(") || sawCall(w.wFake, "RemoveVolume(") {
		t.Errorf("state %s, calls %v", w.state(t), callNames(w.wFake))
	}
	// a standby on the disk's own node is refused by the restore's own rule
	must(t, w.st.OperatedMembers.Delete(ctx, "m-std"))
	w.instanceStandby(t, "w1")
	if _, err := w.orch.ForgetNodeDisk(ctx, adminP(), w.row.ID, "w1"); err == nil || !strings.Contains(err.Error(), "standby") {
		t.Errorf("same-node standby: %v", err)
	}
	if w.state(t) != disk.StateMissing || sawCall(w.wFake, "StopRemove(") {
		t.Errorf("state %s, calls %v", w.state(t), callNames(w.wFake))
	}
}

// DL: step 3 removes a bound object and nothing else; the folder stays.
func TestForgetStep3UsesTheObjectOnlyRemoveAndLeavesTheFolder(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, false, false)
	dev := docker.DeviceOf(disk.BindOpts(w.row.MountPath, w.row.ID, w.v.Name))
	must(t, w.wFake.CreateVolume(ctx, "plain", "local", nil, nil))
	must(t, w.wFake.CreateVolume(ctx, "elsewhere", "local", map[string]string{"type": "none", "o": "bind", "device": "/other"}, nil))
	for name, want := range map[string]bool{"plain": false, "elsewhere": false, "absent": false, w.v.Name: true} {
		got, err := w.orch.volumes.RemoveBoundTo(ctx, "w1", name, dev)
		if err != nil || got != want {
			t.Errorf("RemoveBoundTo(%s) = %v, %v; want %v", name, got, err, want)
		}
	}
	for _, n := range []string{"plain", "elsewhere", "absent"} {
		if sawCall(w.wFake, "RemoveVolume("+n+")") {
			t.Errorf("%s was removed", n)
		}
	}
	if b, err := os.ReadFile(w.file); err != nil || string(b) != "precious" {
		t.Errorf("the folder's data = %q, %v", b, err)
	}
	if w.folders(t) != 1 {
		t.Errorf("folder rows = %d", w.folders(t))
	}
}

// DL: an env with volumes and no tiles still has its volume key locked.
func TestForgetLockSetHasTheEnvVolumeKeysForAnEnvWithoutTiles(t *testing.T) {
	w := newForgetWorld(t, false, false)
	w.lose(t)
	j, err := w.orch.ForgetNodeDisk(context.Background(), adminP(), w.row.ID, "w1")
	must(t, err)
	if !slices.Contains(j.LockSet, "volume:"+w.v.ID) || !slices.Contains(j.LockSet, "disk:"+w.row.ID) || !slices.Contains(j.LockSet, "node:w1") {
		t.Errorf("lock set = %v", j.LockSet)
	}
	w.wait(t, j)
}

// DL: a second press while a forget job is live never cancels it.
func TestForgetSecondPressRefusedWhileAForgetJobIsLive(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, true, false)
	w.lose(t)
	must(t, w.st.Jobs.Create(ctx, store.Job{ID: uuid.NewString(), Kind: string(kindDiskForget), State: job.Running,
		LockSet: store.StringList{"disk:" + w.row.ID}, Payload: "{}", CreatedAt: time.Now()}))
	_, err := w.orch.ForgetNodeDisk(ctx, adminP(), w.row.ID, "w1")
	if _, ok := errs.IsConflict(err); !ok {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if w.state(t) != disk.StateMissing || sawCall(w.wFake, "StopRemove(") {
		t.Errorf("state %s, calls %v", w.state(t), callNames(w.wFake))
	}
}

// DL: while the row says forgetting the standby is not made again; once the
// row is gone it re-seeds on a plain volume of the Docker disk.
func TestForgetStandbyReseedsOnTheDockerDiskAfterTheRowIsDeletedAndNotWhileForgetting(t *testing.T) {
	ctx := context.Background()
	d, row, r := standbyOnDisk(t)
	tl, err := d.orch.tiles.Get(ctx, d.devT.ID)
	must(t, err)
	d.bound(t, d.wFake, row, r.Volume)
	setState(t, d, row, disk.StateForgetting)
	creates := volumeCreates(d.wFake)
	if err := d.orch.createStandby(ctx, tl, r); !isMissing(err) {
		t.Errorf("createStandby while forgetting = %v", err)
	}
	if volumeCreates(d.wFake) != creates {
		t.Errorf("a volume was made while forgetting: %v", callNames(d.wFake))
	}
	setState(t, d, row, disk.StateMissing)
	j, err := d.orch.ForgetNodeDisk(ctx, adminP(), row.ID, "w1")
	if j = d.run(t, j, err); j.State != job.Done {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	must(t, d.orch.createStandby(ctx, tl, r))
	found, dev, err := d.wFake.VolumeDevice(ctx, r.Volume)
	must(t, err)
	if !found || dev != "" {
		t.Errorf("standby volume found %v device %q, want a plain one", found, dev)
	}
}

func TestForgetFencedOldPrimaryInRewindIsNamedInTheJobLog(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, false, false)
	w.lose(t)
	m, err := w.orch.operated.Create(ctx, w.devT.ID, "postgres", "stackr", "")
	must(t, err)
	must(t, w.st.OperatedMembers.Create(ctx, store.OperatedMember{ID: "m-old", InstanceID: m.ID, Role: opmember.Primary, NodeID: "w1",
		Volume: w.v.Name, State: opmember.Running, Phase: opmember.Rewind, Fenced: true, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	j := w.forget(t)
	if j.State != job.Done {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	if log := jobLog(t, w.diskTest, j); !strings.Contains(log, "m-old") || !strings.Contains(log, "rewind") {
		t.Errorf("log = %q", log)
	}
}

// ---- crash and resume: stage the world as it is after step k, press again ----

func (w *forgetWorld) setForgetting(t *testing.T) {
	setState(t, w.diskTest, w.row, disk.StateForgetting)
}

// DL
func TestForgetCrashAfterForgettingRowResumes(t *testing.T) {
	w := newForgetWorld(t, true, true)
	w.setForgetting(t) // step 1 done, nothing else
	if j := w.forget(t); j.State != job.Done {
		t.Fatalf("resume = %s %q", j.State, j.Error)
	}
	if !w.rowGone(t) || !sawCall(w.wFake, "StopRemove("+w.cid) || !sawCall(w.wFake, "RemoveVolume("+w.v.Name+")") {
		t.Errorf("gone %v, calls %v", w.rowGone(t), callNames(w.wFake))
	}
}

// DL
func TestForgetCrashAfterContainersRemovedResumes(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, true, true)
	w.setForgetting(t)
	must(t, w.wFake.StopRemove(ctx, w.cid)) // step 2 done
	if j := w.forget(t); j.State != job.Done {
		t.Fatalf("resume = %s %q", j.State, j.Error)
	}
	if !w.rowGone(t) || !sawCall(w.wFake, "RemoveVolume("+w.v.Name+")") {
		t.Errorf("gone %v, calls %v", w.rowGone(t), callNames(w.wFake))
	}
}

// DL: after step 3 the volume is gone; an ordinary deploy must not make an
// empty one, and the resume still finishes.
func TestForgetCrashAfterObjectsRemovedResumesAndADeployCannotCreateAnEmptyVolume(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, true, true)
	w.setForgetting(t)
	must(t, w.wFake.StopRemove(ctx, w.cid))
	must(t, w.wFake.RemoveVolume(ctx, w.v.Name)) // step 3 done
	creates := volumeCreates(w.wFake)
	j, err := w.orch.Deploy(ctx, w.tl.ID)
	if err == nil {
		if j = w.wait(t, j); j.State != job.Failed {
			t.Errorf("an ordinary deploy = %s %q", j.State, j.Error)
		}
	}
	if volumeCreates(w.wFake) != creates {
		t.Fatalf("a volume was created: %v", callNames(w.wFake))
	}
	if j := w.forget(t); j.State != job.Done {
		t.Fatalf("resume = %s %q", j.State, j.Error)
	}
	if !w.rowGone(t) || len(w.jobsOf(t, string(kindRestore))) == 0 {
		t.Errorf("gone %v, restores %d", w.rowGone(t), len(w.jobsOf(t, string(kindRestore))))
	}
}

// DL
func TestForgetCrashAfterRestoresQueuedResumes(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, true, true)
	w.setForgetting(t)
	must(t, w.wFake.StopRemove(ctx, w.cid))
	must(t, w.wFake.RemoveVolume(ctx, w.v.Name))
	op := disk.WithOp(ctx, w.row.ID, disk.StateForgetting)
	_, _, err := w.orch.restoreThenDeploy(op, w.edge.ID, []store.Tile{w.tl}) // step 4 done
	must(t, err)
	w.wait(t, Job{ID: w.jobsOf(t, string(kindDeploy))[0].ID})
	if j := w.forget(t); j.State != job.Done {
		t.Fatalf("resume = %s %q", j.State, j.Error)
	}
	if !w.rowGone(t) {
		t.Error("the row is still there")
	}
}

// DL
func TestForgettingRowRefusesOrdinaryDeploy(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, true, false)
	w.setForgetting(t)
	creates := volumeCreates(w.wFake)
	j, err := w.orch.Deploy(ctx, w.tl.ID)
	if err == nil {
		if j = w.wait(t, j); j.State != job.Failed {
			t.Errorf("deploy = %s %q", j.State, j.Error)
		}
	}
	if volumeCreates(w.wFake) != creates {
		t.Errorf("a volume was created: %v", callNames(w.wFake))
	}
}

// DL: a failing enqueue keeps the row forgetting; the resume finishes. (The
// failure here is the plain volume's create, which the restore step needs.)
func TestForgetPartialEnqueueFailureKeepsTheRowForgettingAndResumeFinishes(t *testing.T) {
	w := newForgetWorld(t, true, true)
	w.lose(t)
	w.wFake.Err = map[string]error{"CreateVolume("+w.v.Name+")": errors.New("daemon busy")}
	j := w.forget(t)
	if j.State != job.Failed || !strings.Contains(j.Error, "daemon busy") {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	if w.rowGone(t) || w.state(t) != disk.StateForgetting {
		t.Fatalf("gone %v, state %s", w.rowGone(t), w.state(t))
	}
	w.wFake.Edit(func() { w.wFake.Err = nil })
	if j := w.forget(t); j.State != job.Done || !w.rowGone(t) {
		t.Fatalf("resume = %s %q, gone %v", j.State, j.Error, w.rowGone(t))
	}
}

// DL: under the forget's own context the deploy gates see the Docker disk;
// without it they refuse, so the step needs the context.
func TestForgetRestoreStepCanQueueDeploysUnderForgettingOnTheDockerDisk(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, true, true)
	w.setForgetting(t)
	must(t, w.wFake.StopRemove(ctx, w.cid))
	must(t, w.wFake.RemoveVolume(ctx, w.v.Name))
	// without it the volume is "not ready": nothing is restored
	_, _, _ = w.orch.restoreThenDeploy(ctx, w.edge.ID, []store.Tile{w.tl})
	if len(w.jobsOf(t, string(kindRestore))) != 0 {
		t.Error("a restore was queued without the operation context")
	}
	first, _, err := w.orch.restoreThenDeploy(disk.WithOp(ctx, w.row.ID, disk.StateForgetting), w.edge.ID, []store.Tile{w.tl})
	if err != nil || first.ID == "" || len(w.jobsOf(t, string(kindRestore))) != 1 {
		t.Fatalf("under the op: %+v, %v, restores %d", first, err, len(w.jobsOf(t, string(kindRestore))))
	}
}

// ---- the Discard verbs ----

// ghost makes volume data a plain Docker object next to its folder.
func (w *forgetWorld) plainGhost(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	must(t, w.wFake.RemoveVolume(ctx, w.v.Name))
	must(t, w.wFake.CreateVolume(ctx, w.v.Name, "local", nil, nil))
	w.wFake.Edit(func() {
		w.wFake.Volumes = append(w.wFake.Volumes, docker.VolumeInfo{Name: w.v.Name, SizeBytes: 5})
	})
}

func (w *forgetWorld) boundGhost(t *testing.T, device string) {
	t.Helper()
	ctx := context.Background()
	must(t, w.wFake.RemoveVolume(ctx, w.v.Name))
	must(t, w.wFake.CreateVolume(ctx, w.v.Name, "local", map[string]string{"type": "none", "o": "bind", "device": device}, nil))
}

// DL
func TestDiscardPlainCopyRemovesOnlyThePlainObjectAndKeepsTheFolder(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, false, false)
	w.plainGhost(t)
	before := len(w.wFake.Calls())
	must(t, w.orch.DiscardPlainCopy(ctx, adminP(), w.v.ID, w.v.Slug))
	var removed []string
	for _, c := range w.wFake.Calls()[before:] {
		if c.Method == "RemoveVolume" || c.Method == "UntarVolume" || c.Method == "CreateVolume" || c.Method == "StopRemove" {
			removed = append(removed, c.String())
		}
	}
	if !slices.Equal(removed, []string{"RemoveVolume(" + w.v.Name + ")"}) {
		t.Errorf("calls = %v", removed)
	}
	if b, err := os.ReadFile(w.file); err != nil || string(b) != "precious" || w.folders(t) != 1 {
		t.Errorf("folder data %q, %v, rows %d", b, err, w.folders(t))
	}
	evs, err := w.orch.SecurityLog(ctx, SecurityFilter{})
	must(t, err)
	if !slices.ContainsFunc(evs, func(e SecurityEvent) bool { return e.Action == "disk.discard-copy" }) {
		t.Error("no disk.discard-copy in the security log")
	}
}

func TestDiscardPlainCopyNeedsTheTypedConfirm(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, false, false)
	w.plainGhost(t)
	before := countCalls(w.wFake, "RemoveVolume(")
	if err := w.orch.DiscardPlainCopy(ctx, adminP(), w.v.ID, "nope"); !isInvalid(err) {
		t.Errorf("err = %v", err)
	}
	if err := w.orch.DiscardPlainCopy(ctx, nil, w.v.ID, w.v.Slug); err == nil {
		t.Error("no principal went through")
	}
	if countCalls(w.wFake, "RemoveVolume(") != before {
		t.Error("the object was removed")
	}
	// not a ghost: nothing to discard
	other := w.vol(t, "other")
	w.ensure(t, other)
	if err := w.orch.DiscardPlainCopy(ctx, adminP(), other.ID, other.Slug); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("a disk volume with no ghost: %v", err)
	}
	// a bound object is the other verb's
	w.boundGhost(t, "/elsewhere/data")
	if err := w.orch.DiscardPlainCopy(ctx, adminP(), w.v.ID, w.v.Slug); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("a bound object by the plain verb: %v", err)
	}
}

// DL
func TestDiscardBoundObjectRemovesOnlyTheDockerObjectAndShowsNoPaths(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, false, false)
	w.boundGhost(t, "/elsewhere/data")
	err := w.orch.DiscardBoundObject(ctx, adminP(), w.v.ID, "nope")
	if !isInvalid(err) || strings.Contains(err.Error(), "/elsewhere") || strings.Contains(err.Error(), w.row.MountPath) {
		t.Fatalf("confirm refusal = %v, want no host paths", err)
	}
	if sawCall(w.wFake, "RemoveVolume("+w.v.Name+")") && countCalls(w.wFake, "RemoveVolume(") > 1 {
		t.Fatal("removed on a wrong confirm")
	}
	before := len(w.wFake.Calls())
	must(t, w.orch.DiscardBoundObject(ctx, adminP(), w.v.ID, w.v.Slug))
	var removed []string
	for _, c := range w.wFake.Calls()[before:] {
		if c.Method == "RemoveVolume" || c.Method == "StopRemove" || c.Method == "UntarVolume" || c.Method == "CreateVolume" {
			removed = append(removed, c.String())
		}
	}
	if !slices.Equal(removed, []string{"RemoveVolume(" + w.v.Name + ")"}) {
		t.Errorf("calls = %v", removed)
	}
	if b, err := os.ReadFile(w.file); err != nil || string(b) != "precious" || w.folders(t) != 1 {
		t.Errorf("folder data %q, %v, rows %d", b, err, w.folders(t))
	}
}

func TestDiscardBoundObjectNeedsTheTypedConfirm(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, false, false)
	w.boundGhost(t, "/elsewhere/data")
	before := countCalls(w.wFake, "RemoveVolume(")
	for _, c := range []string{"", "other", w.v.Name + "x"} {
		if err := w.orch.DiscardBoundObject(ctx, adminP(), w.v.ID, c); !isInvalid(err) {
			t.Errorf("confirm %q: %v", c, err)
		}
	}
	if countCalls(w.wFake, "RemoveVolume(") != before {
		t.Error("the object was removed")
	}
	// a plain copy is the other verb's
	w.plainGhost(t)
	if err := w.orch.DiscardBoundObject(ctx, adminP(), w.v.ID, w.v.Slug); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("a plain copy by the bound verb: %v", err)
	}
}
