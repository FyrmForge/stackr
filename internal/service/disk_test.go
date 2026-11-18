package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// putMesh is a node's worker that only records the registries pushed to it.
type putMesh struct {
	mu   sync.Mutex
	regs [][]worker.DiskReg
}

func (p *putMesh) Facts(context.Context) (worker.Facts, error)       { return worker.Facts{}, nil }
func (p *putMesh) MeshInfo(context.Context) (worker.MeshInfo, error) { return worker.MeshInfo{}, nil }
func (p *putMesh) ApplyMesh(context.Context, worker.MeshSpec) error  { return nil }
func (p *putMesh) PutDisks(_ context.Context, r []worker.DiskReg) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.regs = append(p.regs, r)
	return nil
}

func (p *putMesh) pushes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.regs)
}

func (p *putMesh) last() []worker.DiskReg {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.regs) == 0 {
		return nil
	}
	return p.regs[len(p.regs)-1]
}

// diskTest is two nodes whose disk commands run in process on a temp root,
// with a mount table the test writes. w1 is the node with the disk.
type diskTest struct {
	*twoNodes
	env    hostdisk.Env
	mounts []string
	put    *putMesh
}

func newDiskTest(t *testing.T, opts ...Option) *diskTest {
	t.Helper()
	d := &diskTest{
		env: hostdisk.Env{Root: t.TempDir(), Mountinfo: filepath.Join(t.TempDir(), "mountinfo")},
		put: &putMesh{},
	}
	d.table(t)
	d.twoNodes = newTwoNodes(t, append([]Option{WithDiskHost(InProcessDisk(d.env))}, opts...)...)
	d.orch.diskNodeOf = func(store.Node) (MeshNode, bool) { return d.put, true }
	d.facts(t, worker.Facts{DisksRead: true})
	return d
}

func (d *diskTest) table(t *testing.T) {
	t.Helper()
	tbl := "25 1 8:1 / / rw - ext4 /dev/sda1 rw\n"
	for i, p := range d.mounts {
		dev, err := hostdisk.DevString(filepath.Join(d.env.Root, p)) // the opened root must be this device
		must(t, err)
		tbl += fmt.Sprintf("%d 25 %s / %s rw - ext4 /dev/sd%c rw\n", 36+i, dev, p, 'b'+i)
	}
	must(t, os.WriteFile(d.env.Mountinfo, []byte(tbl), 0o644))
}

func (d *diskTest) mount(t *testing.T, path string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Join(d.env.Root, path), 0o755))
	d.mounts = append(d.mounts, path)
	d.table(t)
}

func (d *diskTest) unmount(t *testing.T, path string) {
	t.Helper()
	d.mounts = slices.DeleteFunc(d.mounts, func(p string) bool { return p == path })
	d.table(t)
}

// facts sets what w1's worker last said.
func (d *diskTest) facts(t *testing.T, f worker.Facts) {
	t.Helper()
	m := d.orch.mesh
	m.mu.Lock()
	m.facts["w1"] = f
	m.at["w1"] = time.Now()
	m.mu.Unlock()
}

func (d *diskTest) run(t *testing.T, j Job, err error) Job {
	t.Helper()
	must(t, err)
	return d.wait(t, j)
}

// addDisk makes /disk the ready disk of w1.
func (d *diskTest) addDisk(t *testing.T) store.NodeDisk {
	t.Helper()
	d.mount(t, "/disk")
	j, err := d.orch.AddNodeFolder(context.Background(), adminP(), "w1", "/disk")
	if j = d.run(t, j, err); j.State != job.Done {
		t.Fatalf("add disk: %s %s", j.State, j.Error)
	}
	row, ok, err := d.orch.nodeDisk(context.Background(), "w1")
	must(t, err)
	if !ok || row.State != disk.StateReady {
		t.Fatalf("disk row = %+v (has %v)", row, ok)
	}
	return row
}

// folder is where a volume's folder really is.
func (d *diskTest) folder(row store.NodeDisk, name string) string {
	return filepath.Join(d.env.Root, row.MountPath, row.ID, name)
}

// vol declares an env volume of the edge env (on w1).
func (d *diskTest) vol(t *testing.T, slug string) store.Volume {
	t.Helper()
	v, _, err := d.orch.volumes.Declare(context.Background(), volume.Scope{Kind: "env", ID: d.edge.ID}, slug, nil)
	must(t, err)
	return v
}

func (d *diskTest) ensure(t *testing.T, v store.Volume) {
	t.Helper()
	_, err := d.orch.volumes.Ensure(context.Background(), v)
	must(t, err)
}

func setState(t *testing.T, d *diskTest, row store.NodeDisk, state string) {
	t.Helper()
	row.State = state
	must(t, d.st.NodeDisks.Update(context.Background(), row))
}

func volumeCreates(f *dockerfake.Fake) int {
	n := 0
	for _, c := range callNames(f) {
		if strings.HasPrefix(c, "CreateVolume(") {
			n++
		}
	}
	return n
}

func TestAddFolderJobMakesRowAndStamp(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	if row.MountPath != "/disk" || row.NodeID != "w1" || row.Kind != "path" {
		t.Errorf("row = %+v", row)
	}
	if _, err := os.Stat(filepath.Join(d.env.Root, "disk", ".stackr-disk")); err != nil {
		t.Errorf("no stamp: %v", err)
	}
	if st, err := os.Stat(filepath.Join(d.env.Root, "disk", row.ID)); err != nil || !st.IsDir() {
		t.Errorf("no disk folder: %v", err)
	}
	if got := d.put.last(); len(got) != 1 || got[0] != (worker.DiskReg{ID: row.ID, Path: "/disk", Owner: "w1"}) {
		t.Errorf("registry pushed = %+v", got)
	}
	evs, err := d.orch.SecurityLog(ctx, SecurityFilter{})
	must(t, err)
	if !slices.ContainsFunc(evs, func(e SecurityEvent) bool { return e.Action == "disk.add" && e.TargetID == "w1" }) {
		t.Errorf("no disk.add in the security log: %+v", evs)
	}
	if v, err := d.orch.NodeDisk(ctx, "w1"); err != nil || !v.Has || v.State != "ready" || v.Path != "/disk" {
		t.Errorf("NodeDisk = %+v, %v", v, err)
	}
	if _, err := d.orch.AddNodeFolder(ctx, adminP(), "w1", "/other"); err == nil {
		t.Error("a second folder was accepted")
	}
	if _, err := d.orch.AddNodeFolder(ctx, ownerP("o"), "w1", "/disk"); err == nil {
		t.Error("a non-admin added a folder")
	}
}

func TestAddRefusedWhileWorkerDoesNotReportDisksRead(t *testing.T) {
	d := newDiskTest(t)
	d.facts(t, worker.Facts{}) // an old worker: it does not say
	d.mount(t, "/disk")
	_, err := d.orch.AddNodeFolder(context.Background(), adminP(), "w1", "/disk")
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("err = %v, want a refusal that says the worker is too old", err)
	}
	if _, ok, _ := d.orch.nodeDisk(context.Background(), "w1"); ok {
		t.Error("a row was made")
	}
}

// a deploy of a tile that mounts a volume binds the volume to its folder.
func TestDeployOnNodeWithDiskBindsFolder(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	v := d.vol(t, "data")
	tl := d.diskTile(t, "db", "data:/data")
	d.wFake.RunID = "c-db"
	d.wFake.Details = map[string]docker.Detail{"c-db": {Running: true, Networks: map[string]string{d.edge.Network: "10.0.0.2"}}}
	j, err := d.orch.Deploy(ctx, tl.ID)
	if j = d.run(t, j, err); j.State != job.Done {
		t.Fatalf("deploy: %s %s", j.State, j.Error)
	}
	ok, dev, err := d.wFake.VolumeDevice(ctx, v.Name)
	must(t, err)
	if want := "/disk/" + row.ID + "/" + v.Name; !ok || dev != want {
		t.Errorf("volume device = %q (found %v), want %q", dev, ok, want)
	}
	if st, err := os.Stat(d.folder(row, v.Name)); err != nil || !st.IsDir() {
		t.Errorf("no folder: %v", err)
	}
}

func (d *diskTest) diskTile(t *testing.T, name, volumes string) Tile {
	t.Helper()
	tl, err := d.orch.CreateTile(context.Background(), Tile{
		StackID: d.stack, EnvironmentID: d.edge.ID, Name: name, Kind: tile.Image, ImageRef: "nginx:1",
		ContainerPort: 80, Volumes: volumes,
	})
	must(t, err)
	return tl
}

// DL: a stamp that is gone is a missing disk: the deploy fails, the row
// latches, and no Docker volume is made.
func TestDeployWithStampMissingFailsDiskMissingAndCreatesNothing(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	d.vol(t, "data")
	tl := d.diskTile(t, "db", "data:/data")
	must(t, os.Remove(filepath.Join(d.env.Root, "disk", ".stackr-disk")))
	j, err := d.orch.Deploy(ctx, tl.ID)
	if j = d.run(t, j, err); j.State != job.Failed || !strings.Contains(j.Error, "disk is missing") {
		t.Fatalf("deploy: %s %q, want failed with the missing disk", j.State, j.Error)
	}
	if n := volumeCreates(d.wFake); n != 0 {
		t.Errorf("%d volumes created: %v", n, callNames(d.wFake))
	}
	if got, _ := d.st.NodeDisks.Get(ctx, row.ID); got.State != disk.StateMissing {
		t.Errorf("state = %q, want latched missing", got.State)
	}
}

// DL: a disk that is still being added is not "no disk".
func TestDeployWithAddingDiskFailsAndCreatesNothing(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	must(t, d.st.NodeDisks.Create(ctx, store.NodeDisk{ID: "d1", NodeID: "w1", Kind: "path", MountPath: "/disk", State: disk.StateAdding, CreatedAt: time.Now()}))
	d.vol(t, "data")
	tl := d.diskTile(t, "db", "data:/data")
	j, err := d.orch.Deploy(ctx, tl.ID)
	if j = d.run(t, j, err); j.State != job.Failed || !strings.Contains(j.Error, "disk is missing") {
		t.Fatalf("deploy: %s %q", j.State, j.Error)
	}
	if n := volumeCreates(d.wFake); n != 0 {
		t.Errorf("%d volumes created: %v", n, callNames(d.wFake))
	}
}

// scratch is a second env on w1, with no tiles, so it can be deleted.
func (d *diskTest) scratch(t *testing.T, slug string) (store.Environment, store.Volume) {
	t.Helper()
	e, err := d.orch.CreateEnv(context.Background(), d.stack, "tmp", EnvSpec{
		NodeID: "w1", Type: environment.Static, FromKind: environment.FromBranch, FromBranch: "main",
	})
	must(t, err)
	v, _, err := d.orch.volumes.Declare(context.Background(), volume.Scope{Kind: "env", ID: e.ID}, slug, nil)
	must(t, err)
	return e, v
}

// orphanOf deletes the scratch env, which leaves its volume an orphan on w1.
func (d *diskTest) orphanOf(t *testing.T, e store.Environment, v store.Volume) {
	t.Helper()
	must(t, d.orch.DeleteEnv(context.Background(), e.ID))
	got, err := d.orch.volumes.Get(context.Background(), v.ID)
	must(t, err)
	if got.OrphanedAt == nil || got.NodeID != "w1" {
		t.Fatalf("volume = orphaned %v on %q", got.OrphanedAt, got.NodeID)
	}
}

func (d *diskTest) orphans(t *testing.T) string {
	t.Helper()
	local, err := d.orch.localDest(context.Background())
	must(t, err)
	var log bytes.Buffer
	_ = d.orch.backup.Orphans(context.Background(), 0, local, &log)
	return log.String()
}

func (d *diskTest) keptRowAndData(t *testing.T, v store.Volume, file string) {
	t.Helper()
	if _, err := d.orch.volumes.Get(context.Background(), v.ID); err != nil {
		t.Errorf("the orphan row is gone: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("the data is gone: %v", err)
	}
	if sawCall(d.wFake, "RemoveVolume(") {
		t.Errorf("a volume was removed: %v", callNames(d.wFake))
	}
}

// DL: the orphan job on a disk that is not mounted must keep row and data.
func TestOrphanJobOnUnmountedDiskVolumeKeepsRowAndData(t *testing.T) {
	d := newDiskTest(t)
	row := d.addDisk(t)
	e, v := d.scratch(t, "data")
	d.ensure(t, v)
	file := filepath.Join(d.folder(row, v.Name), "x")
	must(t, os.WriteFile(file, []byte("data"), 0o644))
	d.orphanOf(t, e, v)
	d.unmount(t, "/disk")
	d.orphans(t)
	d.keptRowAndData(t, v, file)
}

// DL: latched missing, and Docker has no object for the volume. Docker's
// zero is not "empty".
func TestOrphanJobOnLatchedDiskWithNoObjectKeepsRowAndData(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	e, v := d.scratch(t, "data")
	must(t, d.st.DiskFolders.Create(ctx, store.DiskFolder{ID: "f1", DiskID: row.ID, Name: v.Name, CreatedAt: time.Now()}))
	must(t, os.MkdirAll(d.folder(row, v.Name), 0o755))
	file := filepath.Join(d.folder(row, v.Name), "x")
	must(t, os.WriteFile(file, []byte("data"), 0o644))
	setState(t, d, row, disk.StateMissing)
	d.orphanOf(t, e, v)
	d.orphans(t)
	d.keptRowAndData(t, v, file)
}

func TestOrphanJobBacksUpThenRemovesFolder(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	e, v := d.scratch(t, "data")
	d.ensure(t, v)
	must(t, os.WriteFile(filepath.Join(d.folder(row, v.Name), "x"), []byte("data"), 0o644))
	d.wFake.TarOut = []byte("tar")
	d.orphanOf(t, e, v)
	d.orphans(t)
	if !sawCall(d.wFake, "TarVolume("+v.Name+")") || !sawCall(d.wFake, "RemoveVolume("+v.Name+")") {
		t.Errorf("backup then remove expected: %v", callNames(d.wFake))
	}
	if _, err := os.Stat(d.folder(row, v.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the folder is still there: %v", err)
	}
	if fs, _ := d.st.DiskFolders.ListByDisk(ctx, row.ID); len(fs) != 0 {
		t.Errorf("folder rows left: %+v", fs)
	}
	if _, err := d.orch.volumes.Get(ctx, v.ID); err == nil {
		t.Error("the orphan row is still there")
	}
}

// checkRig: a latched disk with a database tile that mounts a disk volume
// (stopped) and a web tile that does not (running).
func (d *diskTest) checkRig(t *testing.T) (db, web Tile) {
	t.Helper()
	row := d.addDisk(t)
	v := d.vol(t, "data")
	d.ensure(t, v)
	db = d.diskTile(t, "db", "data:/data")
	web = d.edgeT
	d.wFake.Containers = []docker.Container{
		{ID: "c-db", Name: "db", State: "exited", Labels: map[string]string{tile.LabelTile: db.ID, tile.LabelRole: "replica"}},
		{ID: "c-web", Name: "web", State: "running", Labels: map[string]string{tile.LabelTile: web.ID, tile.LabelRole: "replica"}},
	}
	d.unmount(t, "/disk")
	// a deploy finds the disk gone and latches it
	if got, err := d.orch.diskLeaf.Verify(context.Background(), row); err != nil || got.State != disk.StateMissing {
		t.Fatalf("verify on an unmounted disk = %q, %v; want it latched missing", got.State, err)
	}
	d.mount(t, "/disk") // it comes back; nothing sets ready yet
	return db, web
}

func (d *diskTest) deployJobs(t *testing.T, tl Tile) bool {
	t.Helper()
	j, ok, err := d.orch.jobRows.Last(context.Background(), tl.ID)
	must(t, err)
	return ok && j.Kind == string(kindDeploy)
}

func (d *diskTest) check(t *testing.T) Job {
	t.Helper()
	j, err := d.orch.CheckNodeDisk(context.Background(), adminP(), "w1")
	if j = d.run(t, j, err); j.State != job.Done {
		t.Fatalf("check disk: %s %s", j.State, j.Error)
	}
	return j
}

func TestCheckDiskSetsReadyAndDeploysOnlyTilesWithDiskVolumesThatAreNotRunning(t *testing.T) {
	d := newDiskTest(t)
	db, web := d.checkRig(t)
	if row, _, _ := d.orch.nodeDisk(context.Background(), "w1"); row.State != disk.StateMissing {
		t.Fatalf("state = %q, want missing before the check", row.State)
	}
	d.check(t)
	if row, _, _ := d.orch.nodeDisk(context.Background(), "w1"); row.State != disk.StateReady {
		t.Errorf("state = %q, want ready", row.State)
	}
	if !d.deployJobs(t, db) {
		t.Error("the stopped tile with a disk volume was not deployed")
	}
	if d.deployJobs(t, web) {
		t.Error("the running tile with no disk volume was deployed")
	}
	evs, _ := d.orch.SecurityLog(context.Background(), SecurityFilter{})
	if !slices.ContainsFunc(evs, func(e SecurityEvent) bool { return e.Action == "disk.check" }) {
		t.Error("no disk.check in the security log")
	}
}

func TestCheckDiskLeavesAHealthyTileWithoutDiskVolumesAlone(t *testing.T) {
	d := newDiskTest(t)
	_, web := d.checkRig(t)
	d.check(t)
	if d.deployJobs(t, web) {
		t.Error("a healthy tile with no disk volume was deployed")
	}
}

// A disk that comes back stays missing until Check disk: nothing the
// controller reads or pushes sets ready.
func TestNoAutoReadyWhenDiskReturns(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	setState(t, d, row, disk.StateMissing)
	n, _ := d.st.Nodes.Get(ctx, "w1")
	_ = d.orch.reconcileDisks(ctx, n, worker.Facts{DisksRead: true})
	_, _ = d.orch.NodeDisk(ctx, "w1")
	_, _, _ = d.orch.dataDiskOf(ctx, "w1")
	a := diskAdapter{d.orch}
	if st, err := a.State(ctx, "w1"); err != nil || st != volume.DiskNotReady {
		t.Errorf("state = %v, %v", st, err)
	}
	var me *volume.MissingError
	if _, err := a.Place(ctx, "w1", "x"); !errors.As(err, &me) {
		t.Errorf("Place err = %v, want *volume.MissingError", err)
	}
	if _, err := a.Usage(ctx, "w1", volume.UseEmpty, "x"); !errors.As(err, &me) {
		t.Errorf("Usage err = %v, want *volume.MissingError", err)
	}
	if got, _ := d.st.NodeDisks.Get(ctx, row.ID); got.State != disk.StateMissing {
		t.Errorf("state = %q, want missing", got.State)
	}
	d.check(t)
	if got, _ := d.st.NodeDisks.Get(ctx, row.ID); got.State != disk.StateReady {
		t.Errorf("state = %q after Check disk, want ready", got.State)
	}
}

// removable is a node w2 with a disk row; the disk itself is /disk on the rig.
func (d *diskTest) removable(t *testing.T) (*dockerfake.Fake, store.NodeDisk) {
	t.Helper()
	f, _ := seedRemovable(t, d.twoNodes, "w2")
	d.mount(t, "/disk")
	row, err := d.orch.diskLeaf.AddPath(context.Background(), "w2", "/disk", "inst")
	must(t, err)
	f.TarOut = []byte("tar")
	fakeUninstall(t, nil)
	return f, row
}

func (d *diskTest) removeW2(t *testing.T) {
	t.Helper()
	j, err := d.orch.RemoveNode(context.Background(), adminP(), "w2")
	if j = d.run(t, j, err); j.State != job.Done {
		t.Fatalf("remove: %s %s", j.State, j.Error)
	}
}

func TestRemoveNodeDropsDiskRows(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	_, row := d.removable(t)
	_, err := d.orch.diskLeaf.Place(ctx, row, "stackr-vol-folderonly")
	must(t, err)
	d.removeW2(t)
	if _, ok, err := d.orch.nodeDisk(ctx, "w2"); ok || err != nil {
		t.Errorf("disk row after remove: %v, %v", ok, err)
	}
	if fs, _ := d.st.DiskFolders.ListByDisk(ctx, row.ID); len(fs) != 0 {
		t.Errorf("folder rows left: %+v", fs)
	}
	if _, err := os.Stat(d.folder(row, "stackr-vol-folderonly")); err != nil {
		t.Errorf("the folder on the disk was touched: %v", err)
	}
}

func TestRemoveNodeBacksUpAFolderThatHasNoObject(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	f, row := d.removable(t)
	_, err := d.orch.diskLeaf.Place(ctx, row, "stackr-vol-folderonly")
	must(t, err)
	d.removeW2(t)
	names := callNames(f)
	mk := slices.Index(names, "CreateVolume(stackr-vol-folderonly)")
	tar := slices.Index(names, "TarVolume(stackr-vol-folderonly)")
	if mk < 0 || tar < mk {
		t.Errorf("want the object made, then tarred: %v", names)
	}
}

func TestRemoveNodeSkipsDiskHelperContainers(t *testing.T) {
	d := newDiskTest(t)
	f, _ := d.removable(t)
	f.Containers = append(f.Containers, docker.Container{
		ID: "dh1", Name: "stackr-disk-abc123", State: "running",
		Labels: map[string]string{docker.LabelManaged: "true", installspec.LabelRole: diskHelperRole},
	})
	d.removeW2(t)
	if sawCall(f, "Stop(dh1)") || !sawCall(f, "Stop(c1)") {
		t.Errorf("want c1 stopped and the disk helper left: %v", callNames(f))
	}
}

func verify() hostdisk.Request {
	return hostdisk.Request{Op: "verify", Kind: "path", Mount: "/disk", DiskID: "d1", Owner: "w1"}
}

func helperTest(t *testing.T) (*twoNodes, *diskHost) {
	t.Helper()
	n := newTwoNodes(t)
	h := n.orch.dh
	h.image = func(context.Context) (string, error) { return "stackr:test", nil }
	return n, h
}

func TestHelperHasADeadlineAndIsRemovedAfterCancel(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		n, h := helperTest(t)
		h.short = 50 * time.Millisecond
		n.wFake.WaitBlock, n.wFake.RunID = true, "h1"
		start := time.Now()
		_, err := h.Disk(context.Background(), "w1", verify())
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
			t.Fatalf("err = %v after %v, want the deadline", err, time.Since(start))
		}
		if !sawCall(n.wFake, "StopRemove(h1)") {
			t.Errorf("helper not removed: %v", callNames(n.wFake))
		}
		s := n.wFake.Specs[0]
		if !strings.HasPrefix(s.Name, "stackr-disk-") || s.Labels[installspec.LabelRole] != diskHelperRole || !s.Privileged || s.Cmd[1] != "disk" {
			t.Errorf("spec = %+v", s)
		}
		if _, err := hostdisk.DecodeRequest(s.Cmd[2]); err != nil {
			t.Errorf("the request on the command line: %v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		n, h := helperTest(t)
		n.wFake.WaitBlock, n.wFake.RunID = true, "h2"
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(30 * time.Millisecond); cancel() }()
		if _, err := h.Disk(ctx, "w1", verify()); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want canceled", err)
		}
		if !sawCall(n.wFake, "StopRemove(h2)") {
			t.Errorf("helper not removed after the cancel: %v", callNames(n.wFake))
		}
	})
	t.Run("no result line is a crash", func(t *testing.T) {
		n, h := helperTest(t)
		n.wFake.RunID, n.wFake.LogsOut = "h3", "panic: boom"
		if _, err := h.Disk(context.Background(), "w1", verify()); err == nil || !strings.Contains(err.Error(), "crashed") {
			t.Errorf("err = %v, want a crash", err)
		}
	})
	t.Run("a refusal is a result", func(t *testing.T) {
		n, h := helperTest(t)
		n.wFake.RunID = "h4"
		n.wFake.LogsOut = "noise\n" + hostdisk.EncodeResult(hostdisk.Result{Error: &hostdisk.Error{Code: hostdisk.CodeDiskMissing, Message: "gone"}})
		res, err := h.Disk(context.Background(), "w1", verify())
		if err != nil || res.Error == nil || res.Error.Code != hostdisk.CodeDiskMissing {
			t.Errorf("res = %+v, err = %v", res, err)
		}
	})
}

func TestOldDiskHelpersAreSwept(t *testing.T) {
	n, h := helperTest(t)
	role := func(deadline string) map[string]string {
		return map[string]string{installspec.LabelRole: diskHelperRole, labelDiskDeadline: deadline}
	}
	n.wFake.Containers = []docker.Container{
		{ID: "old", Name: "stackr-disk-old", State: "running", Labels: role("1")},
		{ID: "live", Name: "stackr-disk-live", State: "running", Labels: role(fmt.Sprint(time.Now().Add(time.Hour).Unix()))},
	}
	n.wFake.RunID = "h5"
	n.wFake.LogsOut = hostdisk.EncodeResult(hostdisk.Result{Disk: &hostdisk.Fact{}})
	if _, err := h.Disk(context.Background(), "w1", verify()); err != nil {
		t.Fatal(err)
	}
	if !sawCall(n.wFake, "StopRemove(old)") || sawCall(n.wFake, "StopRemove(live)") {
		t.Errorf("want only the old helper swept: %v", callNames(n.wFake))
	}
}

func TestInProcessHostOnlyForTheControllerNode(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	h := n.orch.dh
	if !h.inProcess(ctx, n.ctl.ID) || h.inProcess(ctx, "w1") {
		t.Error("a source-build panel runs the controller in process and a worker never")
	}
	n.orch.cfg.PanelSpec = func(string) ContainerSpec { return ContainerSpec{} }
	n.orch.cfg.Version = "v1.2.3"
	if h.inProcess(ctx, n.ctl.ID) || h.inProcess(ctx, "w1") {
		t.Error("an installed panel uses the helper everywhere")
	}
	n.orch.cfg.FakeHost = "fake"
	if !h.inProcess(ctx, n.ctl.ID) {
		t.Error("a fake host runs the controller in process")
	}
}

// With no disk row the Docker calls are today's: no inspect before a create
// or a tar, one inspect for HoldsData.
func TestNodeWithoutDiskUnchanged(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	v, _, err := n.orch.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: n.edge.ID}, "data", nil)
	must(t, err)
	step := func(f func()) []string {
		before := len(callNames(n.wFake))
		f()
		return callNames(n.wFake)[before:]
	}
	got := step(func() { _, err = n.orch.volumes.Ensure(ctx, v); must(t, err) })
	if !slices.Equal(got, []string{"CreateVolume(" + v.Name + ")"}) {
		t.Errorf("ensure: %v", got)
	}
	got = step(func() { must(t, n.orch.volumes.Tar(ctx, v, &bytes.Buffer{}, false)) })
	if !slices.Equal(got, []string{"TarVolume(" + v.Name + ")"}) {
		t.Errorf("tar: %v", got)
	}
	got = step(func() { n.orch.volumes.HoldsData(ctx, v) })
	if !slices.Equal(got, []string{"InspectVolume(" + v.Name + ")"}) {
		t.Errorf("holds data: %v", got)
	}
}

type failDisks struct{ store.NodeDiskStore }

func (failDisks) GetByNode(context.Context, string) (store.NodeDisk, error) {
	return store.NodeDisk{}, errors.New("db gone")
}

// DL: a failed lookup of the row is a failure, never "no disk": nothing falls
// back to the Docker disk or to a plain volume.
func TestStateLookupErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	d.addDisk(t)
	v := d.vol(t, "data")
	tl := d.diskTile(t, "db", "data:/data")
	d.orch.diskLeaf = disk.New(failDisks{d.st.NodeDisks}, d.st.DiskFolders, InProcessDisk(d.env))
	if _, _, err := d.orch.dataDiskOf(ctx, "w1"); err == nil || !strings.Contains(err.Error(), "db gone") {
		t.Errorf("dataDiskOf err = %v", err)
	}
	if _, _, err := d.orch.nodeDisk(ctx, "w1"); err == nil {
		t.Error("nodeDisk hid the error")
	}
	if _, err := d.orch.volumes.Ensure(ctx, v); err == nil {
		t.Error("ensure went on")
	}
	if !d.orch.volumes.HoldsData(ctx, v) || d.orch.volumes.SizeBytes(ctx, v) != -1 {
		t.Error("holds data must be true and the size unknown")
	}
	// the gate now returns the lookup error at once; a failed job is as good
	j, err := d.orch.Deploy(ctx, tl.ID)
	switch {
	case err != nil:
		if !strings.Contains(err.Error(), "db gone") {
			t.Errorf("deploy err = %v", err)
		}
	default:
		if j = d.wait(t, j); j.State != job.Failed || !strings.Contains(j.Error, "db gone") {
			t.Errorf("deploy: %s %q", j.State, j.Error)
		}
	}
	if n := volumeCreates(d.wFake); n != 0 {
		t.Errorf("%d volumes created", n)
	}
}

func TestDataDiskOf(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	if f, tot, err := d.orch.dataDiskOf(ctx, "w1"); err != nil || f != 0 || tot != 0 {
		t.Errorf("no row = %d, %d, %v; want the Docker disk (none reported here)", f, tot, err)
	}
	row := d.addDisk(t)
	if _, _, err := d.orch.dataDiskOf(ctx, "w1"); !errors.Is(err, errs.ErrRefused) { // a user-facing refusal, not a bare error
		t.Error("a disk nobody read has no numbers")
	}
	d.facts(t, worker.Facts{DisksRead: true, Disks: []hostdisk.Fact{{Path: "/disk", Free: 7, Total: 10}, {Path: "/x", Free: 1, Total: 2}}})
	if f, tot, err := d.orch.dataDiskOf(ctx, "w1"); err != nil || f != 7 || tot != 10 {
		t.Errorf("ready = %d, %d, %v", f, tot, err)
	}
	setState(t, d, row, disk.StateMissing)
	var me *disk.MissingError
	if _, _, err := d.orch.dataDiskOf(ctx, "w1"); !errors.As(err, &me) {
		t.Errorf("latched err = %v, want *disk.MissingError, never the Docker disk", err)
	}
}

func TestReconcileDisksPushesOnlyOnMismatch(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	n, _ := d.st.Nodes.Get(ctx, "w1")
	reg := func(r ...worker.DiskReg) worker.Facts { return worker.Facts{DisksRead: true, DiskRegs: r} }

	// no disk, nothing echoed: nothing to say
	must(t, d.orch.reconcileDisks(ctx, n, reg()))
	if d.put.pushes() != 0 {
		t.Fatalf("pushed with no disks: %d", d.put.pushes())
	}
	row := d.addDisk(t)
	base := d.put.pushes() // the add pushed once
	want := worker.DiskReg{ID: row.ID, Path: "/disk", Owner: "w1"}
	must(t, d.orch.reconcileDisks(ctx, n, reg(want)))
	if d.put.pushes() != base {
		t.Errorf("pushed although the worker holds the registry")
	}
	must(t, d.orch.reconcileDisks(ctx, n, reg())) // a worker that restarted empty
	if d.put.pushes() != base+1 || d.put.last()[0] != want {
		t.Errorf("empty worker: pushes %d, last %+v", d.put.pushes(), d.put.last())
	}
	must(t, d.orch.reconcileDisks(ctx, n, reg(worker.DiskReg{ID: "old", Path: "/old", Owner: "w1"}))) // a stale one
	if d.put.pushes() != base+2 {
		t.Errorf("stale registry not replaced")
	}
	must(t, d.orch.reconcileDisks(ctx, n, worker.Facts{})) // too old to say
	if d.put.pushes() != base+2 {
		t.Errorf("pushed to a worker that does not report disks")
	}
	// the last row of a node is gone: the worker is told to drop it
	must(t, d.st.NodeDisks.Delete(ctx, row.ID))
	must(t, d.orch.reconcileDisks(ctx, n, reg(want)))
	if got := d.put.last(); d.put.pushes() != base+3 || len(got) != 0 {
		t.Errorf("clear: pushes %d, last %+v", d.put.pushes(), got)
	}
}

// cached sizes never run a helper from the read: a miss answers unknown and
// starts one walk, whatever the number of reads.
type countHost struct {
	disk.Host
	mu sync.Mutex
	n  map[string]int
}

func (c *countHost) Disk(ctx context.Context, node string, r hostdisk.Request) (hostdisk.Result, error) {
	c.mu.Lock()
	c.n[r.Op]++
	c.mu.Unlock()
	return c.Host.Disk(ctx, node, r)
}

func (c *countHost) count(op string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[op]
}

func TestCachedSizesStartOneBackgroundWalk(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	ch := &countHost{n: map[string]int{}}
	d.addDisk(t)
	v := d.vol(t, "data")
	ch.Host = InProcessDisk(d.env)
	d.orch.diskLeaf = disk.New(d.st.NodeDisks, d.st.DiskFolders, ch)
	d.ensure(t, v)
	row, _, _ := d.orch.nodeDisk(ctx, "w1")
	must(t, os.WriteFile(filepath.Join(d.folder(row, v.Name), "x"), make([]byte, 100), 0o644))
	a := diskAdapter{d.orch}
	before := ch.count("usage")
	first, err := a.Usage(ctx, "w1", volume.UseCached, v.Name)
	if err != nil || len(first) != 0 {
		t.Fatalf("first read = %+v, %v; want unknown, no wait", first, err)
	}
	for range 5 {
		_, _ = a.Usage(ctx, "w1", volume.UseCached, v.Name)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if u, _ := a.Usage(ctx, "w1", volume.UseCached, v.Name); u[v.Name].Bytes == 100 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	u, _ := a.Usage(ctx, "w1", volume.UseCached, v.Name)
	if u[v.Name].Bytes != 100 {
		t.Errorf("walked size = %+v, want 100 bytes", u)
	}
	if got := ch.count("usage") - before; got != 1 {
		t.Errorf("%d usage walks, want 1", got)
	}
}

func TestCheckDiskSkipsATileTheUserStopped(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	db, _ := d.checkRig(t)
	j, err := d.orch.StopTile(ctx, db.ID)
	if j = d.run(t, j, err); j.State != job.Done {
		t.Fatalf("stop: %s %s", j.State, j.Error)
	}
	d.check(t)
	if d.deployJobs(t, db) {
		t.Error("a tile the user stopped was deployed")
	}
}

// V2: the controller of an installed panel cannot see the host's folders.
func TestFolderDiskRefusedOnTheControllerOfAnInstalledPanel(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	d.mount(t, "/disk")
	d.orch.cfg.PanelSpec = func(string) ContainerSpec { return ContainerSpec{} }
	d.orch.cfg.Version = "1.2.3"
	_, err := d.orch.ProbeNodeFolder(ctx, adminP(), d.ctl.ID, "/disk")
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "Add the disk on a worker node") {
		t.Fatalf("probe on the controller: %v", err)
	}
	if _, err := d.orch.AddNodeFolder(ctx, adminP(), d.ctl.ID, "/disk"); !errors.Is(err, errs.ErrRefused) {
		t.Fatalf("add on the controller: %v", err)
	}
	if p, err := d.orch.ProbeNodeFolder(ctx, adminP(), "w1", "/disk"); err != nil || p.Code != "" {
		t.Fatalf("a worker stays allowed: %+v %v", p, err)
	}
	d.orch.cfg.Version = "" // a source build keeps working
	if _, err := d.orch.ProbeNodeFolder(ctx, adminP(), d.ctl.ID, "/disk"); errors.Is(err, errs.ErrRefused) {
		t.Fatalf("a source build was refused: %v", err)
	}
}

// V3: the controller's tick puts its local worker's registry right, like the
// facts poll does for a worker node.
func TestControllerTickPushesTheLocalWorkersRegistry(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	w := worker.New(worker.Config{StateDir: t.TempDir(), HostRoot: t.TempDir(), DockerSock: "/nonexistent"})
	t.Cleanup(w.Stop)
	d.orch.localWorker = w
	must(t, d.st.NodeDisks.Create(ctx, store.NodeDisk{ID: "cd1", NodeID: d.ctl.ID, Kind: "path", MountPath: "/cdisk", State: disk.StateReady}))
	before := d.put.pushes()
	d.orch.controllerDiskAlert(ctx)
	if d.put.pushes() != before+1 || len(d.put.last()) != 1 || d.put.last()[0].ID != "cd1" {
		t.Fatalf("pushes %d -> %d, last %+v", before, d.put.pushes(), d.put.last())
	}
}

// V5: a slow first pull is not a dead mount, and a node that lost the image is pulled again once.
func TestHelperImagePullIsOutsideTheDeadlineAndRetriedOnce(t *testing.T) {
	t.Run("slow pull", func(t *testing.T) {
		n, h := helperTest(t)
		h.short = 50 * time.Millisecond
		n.wFake.PullFor = 300 * time.Millisecond
		n.wFake.RunID = "h6"
		n.wFake.LogsOut = hostdisk.EncodeResult(hostdisk.Result{Disk: &hostdisk.Fact{}})
		if _, err := h.Disk(context.Background(), "w1", verify()); err != nil {
			t.Fatalf("a slow pull hit the op deadline: %v", err)
		}
	})
	t.Run("image gone", func(t *testing.T) {
		n, h := helperTest(t)
		n.wFake.Err = map[string]error{"Run": docker.ErrNotFound}
		if _, err := h.Disk(context.Background(), "w1", verify()); !errors.Is(err, docker.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
		pulls, runs := 0, 0
		for _, c := range callNames(n.wFake) {
			switch {
			case strings.HasPrefix(c, "Pull("):
				pulls++
			case strings.HasPrefix(c, "Run("):
				runs++
			}
		}
		if pulls != 2 || runs != 2 {
			t.Errorf("pulls %d runs %d, want the image pulled again and one retry: %v", pulls, runs, callNames(n.wFake))
		}
	})
}

// V7: the deploy gate fails closed when the disk row cannot be read.
func TestDiskCheckFailsClosedOnARowLookupError(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	d.addDisk(t)
	d.orch.diskLeaf = disk.New(failDisks{d.st.NodeDisks}, d.st.DiskFolders, InProcessDisk(d.env))
	if _, err := d.orch.diskCheck(ctx, "w1"); err == nil || !strings.Contains(err.Error(), "db gone") {
		t.Fatalf("diskCheck err = %v, want the lookup error", err)
	}
}

// V8: a placement plan measures a disk volume now. The cache is cold here, and
// its unknown (-1) would skip the room check.
func TestPlacementPlanMeasuresADiskVolumeFresh(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	v := d.vol(t, "data")
	_, err0 := d.orch.volumes.Ensure(ctx, v)
	must(t, err0)
	must(t, os.WriteFile(filepath.Join(d.folder(row, v.Name), "f"), make([]byte, 10), 0o644))
	l, err := d.orch.pmLine(ctx, d.edge, false)
	must(t, err)
	if l.Bytes != 10 {
		t.Fatalf("plan line bytes = %d, want 10", l.Bytes)
	}
}

// V8: the pg-upgrade and restore room checks measure a disk volume now. The
// cache is cold here, and its unknown (-1) would pass any amount of free space.
func TestRoomChecksMeasureADiskVolumeFresh(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	v := d.vol(t, "data")
	_, err := d.orch.volumes.Ensure(ctx, v)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(d.folder(row, v.Name), "f"), make([]byte, 10), 0o644))
	d.facts(t, worker.Facts{DisksRead: true, Disks: []hostdisk.Fact{{Path: "/disk", Free: 5, Total: 100}}})
	if err := d.orch.pgupRoom(ctx, pgChange{sv: v}); err == nil {
		t.Errorf("pg-upgrade room with 5 free for 10 bytes: %v", err)
	}
	if err := d.orch.restoreRoom(ctx, pitrSource{vol: v}, backup.Plan{}); err == nil {
		t.Error("restore room with 5 free for 10 bytes passed")
	}
}
