package service

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
	"github.com/FyrmForge/stackr/internal/worker"
)

// Volume paths that bypassed the volume leaf (work package 1R): a member's
// data and archive volumes, the failover specs, the pg_upgrade helper, the
// move helpers. Each must reach the leaf, so a disk volume is bound to its
// folder, never made as a plain Docker volume, never removed behind the disk.

// bound says volume name on f is bound to its folder on the disk, and the
// folder is there.
func (d *diskTest) bound(t *testing.T, f *dockerfake.Fake, row store.NodeDisk, name string) {
	t.Helper()
	ok, dev, err := f.VolumeDevice(context.Background(), name)
	must(t, err)
	if want := filepath.Join(row.MountPath, row.ID, name); !ok || dev != want {
		t.Errorf("volume %s device = %q (found %v), want %q", name, dev, ok, want)
	}
	if st, err := os.Stat(d.folder(row, name)); err != nil || !st.IsDir() {
		t.Errorf("volume %s has no folder: %v", name, err)
	}
}

// standbyOnDisk is the controller's postgres with a standby on w1, whose disk
// is ready.
func standbyOnDisk(t *testing.T) (*diskTest, store.NodeDisk, InstanceMember) {
	t.Helper()
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	pt := d.devT
	pt.Kind, pt.ImageRef = tile.Operated, "postgres:17"
	must(t, d.st.Tiles.Update(ctx, pt))
	in := pgOn(t, d.twoNodes)
	_, err := d.orch.operated.SetMajor(ctx, in, 17)
	must(t, err)
	must(t, d.st.Environments.SetSubnet(ctx, d.dev.ID, "10.208.0.0/24"))
	r, err := d.orch.AddStandby(ctx, adminP(), d.devT.ID, "w1")
	must(t, err)
	return d, row, r
}

func isMissing(err error) bool {
	var a *disk.MissingError
	return errors.As(err, &a) || errors.Is(err, errs.ErrBusy)
}

func TestStandbyVolumeLandsOnItsNodesDisk(t *testing.T) {
	d, row, r := standbyOnDisk(t)
	d.bound(t, d.wFake, row, r.Volume)
	// it is the leaf that made it: the controller (no disk) made nothing
	if sawCall(d.ctlFake, "CreateVolume(") {
		t.Errorf("controller calls: %v", callNames(d.ctlFake))
	}
}

// DL: the standby container is made on m.Volume by name; a missing object or
// an empty plain ghost must not become the standby's empty data directory.
func TestCreateStandbyEnsuresTheVolumeFirst(t *testing.T) {
	ctx := context.Background()
	d, row, r := standbyOnDisk(t)
	tl, err := d.orch.tiles.Get(ctx, d.devT.ID)
	must(t, err)
	must(t, d.wFake.RemoveVolume(ctx, r.Volume))
	must(t, d.orch.createStandby(ctx, tl, r))
	d.bound(t, d.wFake, row, r.Volume)

	must(t, d.wFake.RemoveVolume(ctx, r.Volume))
	must(t, d.wFake.CreateVolume(ctx, r.Volume, "local", nil, nil)) // a plain ghost
	must(t, d.orch.createStandby(ctx, tl, r))
	d.bound(t, d.wFake, row, r.Volume)

	// a missing disk makes no container
	setState(t, d, row, disk.StateMissing)
	creates := countCalls(d.wFake, "Create(")
	if err := d.orch.createStandby(ctx, tl, r); !isMissing(err) {
		t.Errorf("createStandby on a missing disk = %v", err)
	}
	if got := countCalls(d.wFake, "Create("); got != creates {
		t.Errorf("a container was made on a missing disk: %v", callNames(d.wFake))
	}
}

func countCalls(f *dockerfake.Fake, prefix string) int {
	n := 0
	for _, c := range callNames(f) {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// DL: the refresh removes the old container and makes a new one; the volume
// is ensured before the old one goes, so a missing disk leaves it running.
func TestStandbySyncEnsuresBeforeRemakingTheContainer(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	g := newImageRigOn(t, d.twoNodes, true, true)
	must(t, d.wFake.RemoveVolume(ctx, g.m.Volume))
	g.pass()
	if !g.recreated(d.wFake, "rep1", g.m) {
		t.Fatalf("standby not made again: %v", callNames(d.wFake))
	}
	d.bound(t, d.wFake, row, g.m.Volume)

	d2 := newDiskTest(t)
	row2 := d2.addDisk(t)
	g2 := newImageRigOn(t, d2.twoNodes, true, true)
	setState(t, d2, row2, disk.StateMissing)
	g2.pass()
	if sawCall(d2.wFake, "StopRemove(rep1)") {
		t.Errorf("the old standby went with its disk missing: %v", callNames(d2.wFake))
	}
}

// DL: the control-file and pg_rewind containers mount a member's volume by
// name, and the promoted primary's container mounts the standby's data volume
// and archive volume; each is ensured first.
func TestControlAndRewindSpecsEnsureTheVolumeFirst(t *testing.T) {
	ctx := context.Background()
	// the old primary is on w1: the control file is read there
	d := newDiskTest(t)
	row := d.addDisk(t)
	r := newFailRigOn(t, d.twoNodes, true)
	prim, rep0 := r.members()
	r.lsn["ctl"] = "0/5000100" // the standby on the controller has replayed the checkpoint
	vol, err := d.orch.memberVolume(ctx, r.tile(), prim.ID)
	must(t, err)
	f := failover{Target: rep0.ID, Old: prim.ID, OldNode: "w1"}
	must(t, d.orch.foCheck(ctx, r.tile(), r.inst, &f))
	d.bound(t, d.wFake, row, vol)

	// the standby is on w1: it is rewound there
	d2 := newDiskTest(t)
	row2 := d2.addDisk(t)
	r2 := newFailRigOn(t, d2.twoNodes, false)
	_, rep := r2.members()
	must(t, d2.wFake.RemoveVolume(ctx, rep.Volume))
	must(t, d2.orch.rewind(ctx, r2.tile(), rep))
	d2.bound(t, d2.wFake, row2, rep.Volume)

	// foRecreate: the data volume is ensured right before the container
	must(t, d2.wFake.RemoveVolume(ctx, rep.Volume))
	prim2, _ := r2.members()
	f2 := failover{Target: rep.ID, Old: prim2.ID, OldNode: d2.ctl.ID}
	must(t, d2.orch.foRecreate(ctx, r2.tile(), r2.inst, &f2))
	d2.bound(t, d2.wFake, row2, rep.Volume)

	d3 := newDiskTest(t)
	row3 := d3.addDisk(t)
	r3 := newFailRigOn(t, d3.twoNodes, false)
	prim3, rep3 := r3.members()
	setState(t, d3, row3, disk.StateMissing)
	f3 := failover{Target: rep3.ID, Old: prim3.ID, OldNode: d3.ctl.ID}
	if err := d3.orch.foRecreate(ctx, r3.tile(), r3.inst, &f3); !isMissing(err) {
		t.Errorf("foRecreate on a missing disk = %v", err)
	}
	if r3.saw("w1 create stackr-member") {
		t.Error("the promoted primary was made on a missing disk")
	}
}

// DL: the pg_upgrade helper mounts the old and the new volume by name.
func TestPGUpgradeHelperEnsuresOldAndNewVolume(t *testing.T) {
	ctx := docker.WithNode(context.Background(), "w1")
	d := newDiskTest(t)
	row := d.addDisk(t)
	sv, cv := d.vol(t, "old"), d.vol(t, "new")
	u := &pgupRun{o: d.orch, r: &jobs.Run{Log: io.Discard}, m: OperatedInstance{ID: "00000000-0000", AdminUser: "stackr"}, sv: sv, cv: cv, st: pgUp{From: 17}}
	run := mflow.UpgradeRun{Name: "stackr-pgup-run-test", Step: mflow.PGStepUpgrade, OldVol: sv.Name, NewVol: cv.Name, To: 18}
	if _, err := u.helper(ctx, run, 10); err != nil {
		t.Fatal(err)
	}
	d.bound(t, d.wFake, row, sv.Name)
	d.bound(t, d.wFake, row, cv.Name)

	setState(t, d, row, disk.StateMissing)
	before := len(d.wFake.Specs)
	if _, err := u.helper(ctx, run, 10); !isMissing(err) || len(d.wFake.Specs) != before {
		t.Errorf("helper on a missing disk = %v, specs %d -> %d", err, before, len(d.wFake.Specs))
	}
}

// The move flow's Docker handle for a node reaches EnsureNamed on that node.
func TestMoveNodeDockerEnsuresThroughTheLeaf(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	v := d.vol(t, "data")
	dk, err := moveNodes{d.orch}.Docker("w1")
	must(t, err)
	must(t, dk.EnsureVolume(ctx, v.Name, map[string]string{"stackr.volume": v.ID}))
	d.bound(t, d.wFake, row, v.Name)
	ck, err := moveNodes{d.orch}.Docker(d.ctl.ID)
	must(t, err)
	must(t, ck.EnsureVolume(ctx, "plain", nil))
	if ok, dev, _ := d.ctlFake.VolumeDevice(ctx, "plain"); !ok || dev != "" {
		t.Errorf("the controller's volume = %q (found %v), want plain", dev, ok)
	}
}

func TestDropMemberRemovesFolder(t *testing.T) {
	ctx := context.Background()
	d, row, r := standbyOnDisk(t)
	d.bound(t, d.wFake, row, r.Volume)
	must(t, d.orch.RemoveStandby(ctx, r.ID))
	if ok, _, _ := d.wFake.VolumeDevice(ctx, r.Volume); ok {
		t.Error("the Docker volume is still there")
	}
	if _, err := os.Stat(d.folder(row, r.Volume)); !os.IsNotExist(err) {
		t.Errorf("the folder is still there: %v", err)
	}
	fs, err := d.st.DiskFolders.ListByDisk(ctx, row.ID)
	must(t, err)
	for _, f := range fs {
		if f.Name == r.Volume {
			t.Errorf("folder row %+v outlived the member", f)
		}
	}
}

// DL: a failed archive remove was dropped with `_ =`; it must stop the drop,
// or the folder leaks with no row to find it by.
func TestDropMemberReturnsTheArchiveRemoveError(t *testing.T) {
	ctx := context.Background()
	d, row, r := standbyOnDisk(t)
	arch := mflow.MemberArchiveVolume(r)
	must(t, d.orch.volumes.EnsureNamed(ctx, "w1", arch, map[string]string{LabelMember: r.ID}))
	d.bound(t, d.wFake, row, arch)
	d.wFake.Err = map[string]error{"RemoveVolume(" + arch + ")": errors.New("volume in use")}
	if err := d.orch.RemoveStandby(ctx, r.ID); err == nil || !strings.Contains(err.Error(), "volume in use") {
		t.Fatalf("RemoveStandby = %v, want the archive remove error", err)
	}
	got, err := d.orch.members.Get(ctx, r.ID)
	if err != nil || got.State == "" {
		t.Errorf("the row is gone while its archive folder is not: %v", err)
	}
	if _, err := os.Stat(d.folder(row, arch)); err != nil {
		t.Errorf("the archive folder: %v", err)
	}
}

// DL: the tile's archive volume is emptied by removing it; on a disk the
// folder keeps its old WAL unless the folder goes too.
func TestCleanSpoolEmptiesDiskVolume(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	et := d.diskTile(t, "db", "")
	et.Slug = "db" // the store row's slug is not what the test rig sets
	sv := d.vol(t, mflow.ArchiveSlug(et.Slug))
	d.ensure(t, sv)
	seg := filepath.Join(d.folder(row, sv.Name), "000000010000000000000007")
	must(t, os.WriteFile(seg, []byte("wal"), 0o644))
	p := InstanceMember{ID: "11111111-2222", NodeID: "w1", Volume: "member-vol"}
	must(t, d.orch.cleanSpool(ctx, et, p))
	if _, err := os.Stat(seg); !os.IsNotExist(err) {
		t.Errorf("the stale segment survived: %v", err)
	}
	if ok, _, _ := d.wFake.VolumeDevice(ctx, sv.Name); ok {
		t.Error("the archive volume object is still there")
	}
}

// The swap exchanges rows only; folder rows are keyed by Docker name, so both
// members' folders keep their rows and stay bound.
func TestFailoverSwapKeepsFolderTracking(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	r := newFailRigOn(t, d.twoNodes, false) // the standby is on w1
	_, err := doSwitchover(d.orch, ctx, adminP(), r.inst.TileID, "w1")
	must(t, err)
	p, _ := r.members() // the promoted standby, now the primary on w1
	if p.NodeID != "w1" || p.Volume == "" {
		t.Fatalf("primary row = %+v", p)
	}
	d.bound(t, d.wFake, row, p.Volume)
	fs, err := d.st.DiskFolders.ListByDisk(ctx, row.ID)
	must(t, err)
	found := false
	for _, f := range fs {
		found = found || f.Name == p.Volume
	}
	if !found {
		t.Errorf("folder rows %+v lost %s", fs, p.Volume)
	}
}

// The dry run's scratch copy is a full data copy: on a node with a disk it is
// made there (not on Docker's disk) and removed, folder and row, at the end.
func TestPGUpgradeScratchOnDiskAndRemoved(t *testing.T) {
	ctx := context.Background()
	env := hostdisk.Env{Root: t.TempDir(), Mountinfo: filepath.Join(t.TempDir(), "mountinfo"), TempMounts: true}
	must(t, os.MkdirAll(filepath.Join(env.Root, "disk"), 0o755))
	dev, err := hostdisk.DevString(filepath.Join(env.Root, "disk"))
	must(t, err)
	must(t, os.WriteFile(env.Mountinfo, []byte("25 1 8:1 / / rw - ext4 /dev/sda1 rw\n36 25 "+dev+" / /disk rw - ext4 /dev/sdb rw\n"), 0o644))
	r := newPGUpRig(t, WithDiskHost(InProcessDisk(env)))
	ctl := storetest.Controller(t, r.st)
	row, err := r.orch.diskLeaf.AddPath(ctx, ctl.ID, "/disk", "inst")
	must(t, err)
	row, err = r.orch.diskLeaf.Verify(ctx, row)
	must(t, err)
	if row.State != disk.StateReady {
		t.Fatalf("disk = %+v", row)
	}
	r.orch.ensureMesh(ctx)
	m := r.orch.mesh
	m.mu.Lock()
	m.facts[ctl.ID] = worker.Facts{DisksRead: true, Disks: []hostdisk.Fact{{Path: "/disk", Mounted: true, Free: 1 << 40, Total: 1 << 41}}}
	m.at[ctl.ID] = time.Now()
	m.mu.Unlock()

	u, nctx := r.bare(t)
	must(t, u.check(nctx, 18))
	scratch := "stackr-pgupcheck-" + u.m.ID[:8]
	var made *dockerfake.VolumeCreate
	for i, c := range r.fake.Created {
		if c.Name == scratch {
			made = &r.fake.Created[i]
		}
	}
	if made == nil || docker.DeviceOf(made.Opts) == "" {
		t.Fatalf("scratch volume = %+v, want bound to the disk", made)
	}
	if ok, _, _ := r.fake.VolumeDevice(ctx, scratch); ok {
		t.Error("the scratch volume object outlived the check")
	}
	if _, err := os.Stat(filepath.Join(env.Root, "disk", row.ID, scratch)); !os.IsNotExist(err) {
		t.Errorf("the scratch folder outlived the check: %v", err)
	}
	fs, err := r.st.DiskFolders.ListByDisk(ctx, row.ID)
	must(t, err)
	for _, f := range fs {
		if f.Name == scratch {
			t.Errorf("scratch folder row %+v outlived the check", f)
		}
	}
}

// A target whose disk is not ready is no room: the plan's disk check fails
// with the reason, it does not read Docker's disk instead.
func TestPlacementDiskCheckRefusesAMissingTargetDisk(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	setState(t, d, row, disk.StateMissing)
	c := d.orch.pmDisk(ctx, d.ctl.ID, "w1", false, false, []store.Volume{d.vol(t, "data")}, d.orch.pmNodeNames(ctx))
	if c.OK || !strings.Contains(c.Detail, "disk is missing") {
		t.Errorf("check = %+v", c)
	}
}

// diskDataEnv is the edge env (on w1) with a volume that holds a file on the
// ready disk, and its Docker object.
func diskDataEnv(t *testing.T) (*diskTest, store.NodeDisk, store.Volume, string) {
	t.Helper()
	d := newDiskTest(t)
	row := d.addDisk(t)
	v := d.vol(t, "data")
	d.ensure(t, v)
	file := filepath.Join(d.folder(row, v.Name), "PG_VERSION")
	must(t, os.WriteFile(file, []byte("17"), 0o644))
	return d, row, v, file
}

// DL: a volume that holds data on the disk is never restored over.
func TestRestoreThenDeployDoesNotRestoreOverDiskData(t *testing.T) {
	ctx := context.Background()
	d, _, v, file := diskDataEnv(t)
	_, notes, err := d.orch.restoreThenDeploy(ctx, d.edge.ID, nil)
	must(t, err)
	if !strings.Contains(strings.Join(notes, "\n"), v.Slug+" holds data") {
		t.Errorf("notes = %v, want %s left alone", notes, v.Slug)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("the data is gone: %v", err)
	}
	if sawCall(d.wFake, "UntarVolume(") || sawCall(d.wFake, "RemoveVolume(") {
		t.Errorf("a restore ran: %v", callNames(d.wFake))
	}
}

// DL: on a latched disk the volume is not "empty" (its object may be gone too):
// nothing is restored and nothing is made.
func TestRestoreThenDeployOnLatchedDiskReturnsMissingAndRestoresNothing(t *testing.T) {
	ctx := context.Background()
	d, row, v, file := diskDataEnv(t)
	setState(t, d, row, disk.StateMissing)
	must(t, d.wFake.RemoveVolume(ctx, v.Name)) // the object is gone as well
	creates := volumeCreates(d.wFake)
	_, notes, err := d.orch.restoreThenDeploy(ctx, d.edge.ID, nil)
	if err != nil && !isMissing(err) {
		t.Fatalf("restoreThenDeploy = %v", err)
	}
	if err == nil && !strings.Contains(strings.Join(notes, "\n"), v.Slug+" holds data") {
		t.Errorf("notes = %v, want %s left alone", notes, v.Slug)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("the data is gone: %v", err)
	}
	if sawCall(d.wFake, "UntarVolume(") || volumeCreates(d.wFake) != creates {
		t.Errorf("something was restored or made: %v", callNames(d.wFake))
	}
}

// DL: the promoted primary's container mounts the standby's data volume by
// name. After a crash that removed the old container, with the volume object
// gone, the folder is rebound first; on a missing disk no container is made or
// started on a plain empty volume.
func TestPromotedPrimaryAfterACrashRebindsTheFolderAndNeverStartsOnAnEmptyVolume(t *testing.T) {
	ctx := context.Background()
	var made int // volume creates before the step under test
	crash := func(t *testing.T, state string) (*failRig, *diskTest, store.NodeDisk, InstanceMember, error) {
		d := newDiskTest(t)
		row := d.addDisk(t)
		r := newFailRigOn(t, d.twoNodes, false)
		prim, rep := r.members()
		if state != "" {
			setState(t, d, row, state)
		}
		s := r.sims["w1"]
		s.mu.Lock() // the standby container is already gone
		s.cs = nil
		s.mu.Unlock()
		must(t, d.wFake.RemoveVolume(ctx, rep.Volume))
		made = volumeCreates(d.wFake)
		f := failover{Target: rep.ID, Old: prim.ID, OldNode: d.ctl.ID}
		return r, d, row, rep, d.orch.foRecreate(ctx, r.tile(), r.inst, &f)
	}
	r, d, row, rep, err := crash(t, "")
	must(t, err)
	d.bound(t, d.wFake, row, rep.Volume)
	if !r.saw("w1 start stackr-member") {
		t.Error("the promoted primary was not started")
	}
	r, d, _, _, err = crash(t, disk.StateMissing)
	if !isMissing(err) {
		t.Errorf("foRecreate on a missing disk = %v", err)
	}
	if r.saw("w1 create") || r.saw("w1 start") || volumeCreates(d.wFake) != made {
		t.Errorf("a container or plain volume was made: %v %v", r.events, callNames(d.wFake))
	}
}

// foRecreate ensures the data mount of PromotedSpec, not only the archive.
func TestFoRecreateEnsuresThePromotedVolumeBeforeCreatingTheContainer(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	r := newFailRigOn(t, d.twoNodes, false)
	prim, rep := r.members()
	must(t, d.wFake.RemoveVolume(ctx, rep.Volume))
	f := failover{Target: rep.ID, Old: prim.ID, OldNode: d.ctl.ID}
	must(t, d.orch.foRecreate(ctx, r.tile(), r.inst, &f))
	var spec docker.ContainerSpec
	for _, s := range r.specs {
		if s.Labels[tile.LabelTile] != "" && s.Labels[LabelMember] == rep.ID {
			spec = s
		}
	}
	if len(spec.Volumes) == 0 || !strings.HasPrefix(spec.Volumes[0], rep.Volume+":") {
		t.Fatalf("promoted spec volumes = %v, want the data volume first", spec.Volumes)
	}
	d.bound(t, d.wFake, row, rep.Volume)
}

// A move onto a node with a disk makes the target volume on the disk.
func TestEnvMoveTargetOnDisk(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	admin := &Principal{User: User{ID: "u-admin"}, Access: authz.User{ID: "u-admin", Active: true, Admin: true}}
	v, _, err := d.orch.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: d.dev.ID}, "data", nil)
	must(t, err)
	d.ctlFake.TarOut = []byte("tar")
	d.wFake.RunID = "new"
	d.wFake.Details = map[string]docker.Detail{"new": {Running: true, Networks: map[string]string{d.dev.Network: "10.209.0.2"}}}
	j, err := d.startMove(admin, d.dev.ID, "w1", false)
	must(t, err)
	if j = d.wait(t, j); j.State != job.Done {
		t.Fatalf("move = %s %q\nw: %v", j.State, j.Error, callNames(d.wFake))
	}
	d.bound(t, d.wFake, row, v.Name)
	if ok, dev, _ := d.ctlFake.VolumeDevice(ctx, v.Name); !ok || dev != "" {
		t.Errorf("the source volume = %q (found %v), want plain", dev, ok)
	}
}

// The plan's disk check reads the target's own disk, and sizes from the leaf.
func TestPMDiskReadsNodeDiskAndLeafSizes(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	d.addDisk(t)
	v, _, err := d.orch.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: d.dev.ID}, "data", nil)
	must(t, err)
	d.ctlFake.Volumes = []docker.VolumeInfo{{Name: v.Name, SizeBytes: 5 << 30}}
	facts := func(diskFree uint64) {
		d.facts(t, worker.Facts{DisksRead: true, DiskFree: 1 << 40, DiskTotal: 1 << 41,
			Disks: []hostdisk.Fact{{Path: "/disk", Mounted: true, Free: diskFree, Total: 1 << 40}}})
	}
	check := func() planfile.Check {
		return d.orch.pmDisk(ctx, d.ctl.ID, "w1", false, false, []store.Volume{v}, d.orch.pmNodeNames(ctx))
	}
	facts(1 << 30) // Docker's disk is huge, the node's disk is not
	if c := check(); c.OK {
		t.Errorf("a 1 GB disk took 5 GB: %+v", c)
	}
	facts(1 << 39)
	if c := check(); !c.OK || strings.Contains(c.Detail, "unknown") {
		t.Errorf("a roomy disk refused: %+v", c)
	}
}
