package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// adoptWorld: the disk of a node that died ("old", forgotten) is mounted at
// /disk on w1, which has no disk of its own. The disk holds volume "data"
// (with a file) of the edge env, which still points at the dead node; volume
// "legacy" of that env never moved to the disk and has a backup. The dead
// node's Docker objects are gone.
type adoptWorld struct {
	*diskTest
	row    store.NodeDisk
	data   store.Volume
	legacy store.Volume
	tl     Tile
	file   string
	n0     int // len(wFake.Created) when the world was built
}

func newAdoptWorld(t *testing.T) *adoptWorld {
	t.Helper()
	ctx := context.Background()
	d := newDiskTest(t)
	w := &adoptWorld{diskTest: d, row: d.addDisk(t)}
	w.data = d.vol(t, "data")
	d.ensure(t, w.data)
	w.file = filepath.Join(d.folder(w.row, w.data.Name), "DATA")
	must(t, os.WriteFile(w.file, []byte("precious"), 0o644))
	w.legacy = d.vol(t, "legacy")
	w.tl = d.diskTile(t, "db", "data:/data\nlegacy:/legacy")
	must(t, d.st.BackupDests.Create(ctx, store.BackupDest{ID: "dest", Kind: "s3", Name: "dest", Endpoint: "e", Bucket: "b", AccessKey: "a", SecretKey: "s", CreatedAt: time.Now()}))
	lid := w.legacy.ID
	must(t, d.st.BackupRuns.Create(ctx, store.BackupRun{ID: "run1", Kind: "volume", VolumeID: &lid, DestID: "dest", Status: backup.Done, ObjectKey: "k.tar.age", CreatedAt: time.Now()}))
	// the node dies: its disk row, stamp and env name it, it has no node row
	w.setOwner(t, "old")
	w.row.NodeID = "old"
	must(t, d.st.NodeDisks.Update(ctx, w.row))
	must(t, d.st.ForgottenNodes.Create(ctx, store.ForgottenNode{ID: "old", Name: "oldbox", ForgottenAt: time.Now()}))
	must(t, d.st.Environments.SetNode(ctx, d.edge.ID, "old"))
	must(t, d.wFake.RemoveVolume(ctx, w.data.Name))
	d.wFake.Edit(func() { d.wFake.Containers = nil })
	w.n0 = len(d.wFake.Created)
	return w
}

func (w *adoptWorld) stampPath() string { return filepath.Join(w.env.Root, "disk", ".stackr-disk") }

func (w *adoptWorld) setStamp(t *testing.T, k, v string) {
	t.Helper()
	b, err := os.ReadFile(w.stampPath())
	must(t, err)
	m := map[string]any{}
	must(t, json.Unmarshal(b, &m))
	m[k] = v
	b, _ = json.Marshal(m)
	must(t, os.WriteFile(w.stampPath(), b, 0o644))
}

func (w *adoptWorld) setOwner(t *testing.T, owner string) { w.setStamp(t, "owner", owner) }

func (w *adoptWorld) stampOwner(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(w.stampPath())
	must(t, err)
	var s hostdisk.Stamp
	must(t, json.Unmarshal(b, &s))
	return s.Owner
}

func (w *adoptWorld) get(t *testing.T) store.NodeDisk {
	t.Helper()
	d, err := w.st.NodeDisks.Get(context.Background(), w.row.ID)
	must(t, err)
	return d
}

func (w *adoptWorld) press(t *testing.T) Job {
	t.Helper()
	j, err := w.orch.AdoptDisk(context.Background(), adminP(), "w1", w.row.ID, "/disk", "w1")
	return w.run(t, j, err)
}

func (w *adoptWorld) pressErr(t *testing.T) error {
	t.Helper()
	_, err := w.orch.AdoptDisk(context.Background(), adminP(), "w1", w.row.ID, "/disk", "w1")
	return err
}

func (w *adoptWorld) done(t *testing.T) {
	t.Helper()
	if j := w.press(t); j.State != job.Done {
		t.Fatalf("adopt = %s %q", j.State, j.Error)
	}
}

func (w *adoptWorld) envNode(t *testing.T) string {
	t.Helper()
	e, err := w.st.Environments.Get(context.Background(), w.edge.ID)
	must(t, err)
	return e.NodeID
}

func (w *adoptWorld) folders(t *testing.T) []string {
	t.Helper()
	fs, err := w.st.DiskFolders.ListByDisk(context.Background(), w.row.ID)
	must(t, err)
	var out []string
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

// createsOf are the CreateVolume calls on w1 since the world was built.
func (w *adoptWorld) createsOf(name string) (bound, plain int) {
	for _, c := range w.wFake.Created[w.n0:] {
		if c.Name != name {
			continue
		}
		if c.Opts == nil {
			plain++
		} else {
			bound++
		}
	}
	return
}

func (w *adoptWorld) jobsOf(t *testing.T, kind string) []store.Job {
	t.Helper()
	rs, err := w.st.Jobs.ListByState(context.Background(), job.Queued, job.Running, job.Done, job.Failed, job.Waiting)
	must(t, err)
	return slices.DeleteFunc(rs, func(j store.Job) bool { return j.Kind != kind })
}

// onlyClaimedFolders: no stray row is left; the claimed volumes' rows are (the
// restored legacy volume is placed on the disk too).
func (w *adoptWorld) onlyClaimedFolders(t *testing.T) {
	t.Helper()
	want := []string{w.data.Name, w.legacy.Name}
	slices.Sort(want)
	if fs := w.folders(t); !slices.Equal(fs, want) {
		t.Errorf("folder rows = %v, want %v", fs, want)
	}
}

func (w *adoptWorld) unchanged(t *testing.T) {
	t.Helper()
	r := w.get(t)
	if r.State != disk.StateReady || r.NodeID != "old" || r.OpNodeID != "" || w.envNode(t) != "old" || w.stampOwner(t) != "old" {
		t.Errorf("something changed: %+v env %s stamp %s", r, w.envNode(t), w.stampOwner(t))
	}
}

// the steps of the job, by hand, so a crash can be stood in front of any of them

func (w *adoptWorld) step2(t *testing.T) store.NodeDisk {
	t.Helper()
	ids := []string{w.edge.ID}
	d, err := w.orch.diskLeaf.BeginAdopt(context.Background(), w.row.ID, "w1", "/disk", ids)
	must(t, err)
	return d
}

func (w *adoptWorld) step3(t *testing.T, d store.NodeDisk) {
	t.Helper()
	must(t, w.orch.diskLeaf.Claim(context.Background(), d))
}

func (w *adoptWorld) step4(t *testing.T, d store.NodeDisk) store.NodeDisk {
	t.Helper()
	n, err := w.st.Nodes.Get(context.Background(), "w1")
	must(t, err)
	d, err = w.orch.adoptCommit(context.Background(), d, n, &jobs.Run{Log: io.Discard})
	must(t, err)
	return d
}

func (w *adoptWorld) stranded(t *testing.T) {
	t.Helper()
	d := w.step2(t)
	w.step3(t, d)
	w.step4(t, d)
}

func TestAdoptFromAForgottenNodeMovesTheDiskAndTheEnv(t *testing.T) {
	w := newAdoptWorld(t)
	w.done(t)
	r := w.get(t)
	if r.State != disk.StateReady || r.NodeID != "w1" || r.OpNodeID != "" || r.OpFromNodeID != "" || r.OpMountPath != "" || r.OpEnvIDs != "" {
		t.Fatalf("row = %+v", r)
	}
	if w.envNode(t) != "w1" || w.stampOwner(t) != "w1" {
		t.Errorf("env on %s, stamp owner %s", w.envNode(t), w.stampOwner(t))
	}
	if d, ok, err := w.orch.nodeDisk(context.Background(), "w1"); err != nil || !ok || d.ID != w.row.ID {
		t.Errorf("w1's disk = %+v %v %v", d, ok, err)
	}
	evs, err := w.orch.SecurityLog(context.Background(), SecurityFilter{})
	must(t, err)
	if !slices.ContainsFunc(evs, func(e SecurityEvent) bool { return e.Action == "disk.adopt" }) {
		t.Error("no disk.adopt event")
	}
}

// DL: the old node's own verify fails from the claim on, the new node's passes.
func TestAdoptRewritesTheStampOwnerAndTheOldNodesVerifyFails(t *testing.T) {
	w := newAdoptWorld(t)
	w.done(t)
	req := hostdisk.Request{Op: "verify", Kind: "path", DiskID: w.row.ID, Mount: "/disk", Owner: "old"}
	if res := hostdisk.Run(context.Background(), w.env, req); res.Error == nil || res.Error.Code != hostdisk.CodeForeignStamp {
		t.Errorf("old node's verify = %+v", res.Error)
	}
	req.Owner = "w1"
	if res := hostdisk.Run(context.Background(), w.env, req); res.Error != nil {
		t.Errorf("new node's verify = %+v", res.Error)
	}
}

// DL: the folder keeps its data and the volume object is made again, bound to it.
func TestAdoptRecreatesVolumeObjectsAndKeepsData(t *testing.T) {
	w := newAdoptWorld(t)
	w.done(t)
	if b, err := os.ReadFile(w.file); err != nil || string(b) != "precious" {
		t.Fatalf("data = %q %v", b, err)
	}
	if bound, plain := w.createsOf(w.data.Name); bound == 0 || plain != 0 {
		t.Errorf("data volume: %d bound, %d plain creates", bound, plain)
	}
	found, dev, err := w.wFake.VolumeDevice(context.Background(), w.data.Name)
	if err != nil || !found || dev != docker.DeviceOf(disk.BindOpts("/disk", w.row.ID, w.data.Name)) {
		t.Errorf("object = %v %q %v", found, dev, err)
	}
}

// DL: a volume that never moved to the disk lived on the dead node's Docker
// disk: it comes back from its backup, and never starts empty.
func TestAdoptRestoresVolumesNotOnTheDiskFromBackup(t *testing.T) {
	w := newAdoptWorld(t)
	w.done(t)
	if len(w.jobsOf(t, string(kindRestore))) != 1 || len(w.jobsOf(t, string(kindDeploy))) == 0 {
		t.Errorf("restore jobs %d, deploy jobs %d", len(w.jobsOf(t, string(kindRestore))), len(w.jobsOf(t, string(kindDeploy))))
	}
}

// DL: while the row is adopting an ordinary deploy gets the missing error and
// creates nothing, so no volume starts empty.
func TestAdoptOrdinaryDeployOnTheNewNodeWhileAdoptingGetsTheMissingErrorAndCreatesNothing(t *testing.T) {
	w := newAdoptWorld(t)
	w.stranded(t)
	n := len(w.wFake.Created)
	j, err := w.orch.Deploy(context.Background(), w.tl.ID)
	if j = w.run(t, j, err); j.State != job.Failed || !strings.Contains(j.Error, "disk is missing") {
		t.Fatalf("deploy = %s %q", j.State, j.Error)
	}
	var me *volume.MissingError
	if _, err := w.orch.volumes.Ensure(context.Background(), w.legacy); !errors.As(err, &me) {
		t.Errorf("ensure of the legacy volume = %v", err)
	}
	if len(w.wFake.Created) != n {
		t.Errorf("volumes created: %+v", w.wFake.Created[n:])
	}
}

// DL: a volume that holds no data is never started empty: with the adopt
// stopped before its restores, no plain or bound object of it exists.
func TestAdoptDoesNotStartALegacyVolumeEmpty(t *testing.T) {
	w := newAdoptWorld(t)
	w.stranded(t)
	if bound, plain := w.createsOf(w.legacy.Name); bound+plain != 0 {
		t.Errorf("legacy volume created: %d %d", bound, plain)
	}
}

func TestAdoptDialogListsRestoredFromBackupOfDateOrStartsEmpty(t *testing.T) {
	w := newAdoptWorld(t)
	bare := w.vol(t, "bare") // no backup
	p, err := w.orch.PlanDiskAdopt(context.Background(), "w1", "/disk")
	must(t, err)
	if p.Refusal != "" || p.DiskID != w.row.ID || p.OldNode != "oldbox" || p.Node != "w1" {
		t.Fatalf("plan = %+v", p)
	}
	if len(p.Envs) != 1 || p.Envs[0].ID != w.edge.ID {
		t.Errorf("envs = %+v", p.Envs)
	}
	if len(p.OnDisk) != 1 || p.OnDisk[0].ID != w.data.ID {
		t.Errorf("on disk = %+v", p.OnDisk)
	}
	var legacy, none *AdoptVolume
	for i, v := range p.Elsewhere {
		switch v.ID {
		case w.legacy.ID:
			legacy = &p.Elsewhere[i]
		case bare.ID:
			none = &p.Elsewhere[i]
		}
	}
	if legacy == nil || !legacy.HasBackup || legacy.Backup.IsZero() || none == nil || none.HasBackup {
		t.Errorf("elsewhere = %+v", p.Elsewhere)
	}
	w.unchanged(t) // a plan writes nothing
}

// DL: folders no volume of the old node claims are kept on the disk, only
// their rows go.
func TestAdoptListsUnclaimedFoldersAndDeletesNone(t *testing.T) {
	ctx := context.Background()
	w := newAdoptWorld(t)
	must(t, os.MkdirAll(filepath.Join(w.env.Root, "disk", w.row.ID, "stray"), 0o755))
	must(t, os.WriteFile(filepath.Join(w.env.Root, "disk", w.row.ID, "stray", "X"), []byte("x"), 0o644))
	must(t, w.st.DiskFolders.Create(ctx, store.DiskFolder{ID: "f-stray", DiskID: w.row.ID, Name: "stray", CreatedAt: time.Now()}))
	p, err := w.orch.PlanDiskAdopt(ctx, "w1", "/disk")
	must(t, err)
	if !slices.Equal(p.Unclaimed, []string{"stray"}) {
		t.Fatalf("unclaimed = %v", p.Unclaimed)
	}
	w.done(t)
	if b, err := os.ReadFile(filepath.Join(w.env.Root, "disk", w.row.ID, "stray", "X")); err != nil || string(b) != "x" {
		t.Errorf("the stray folder was touched: %q %v", b, err)
	}
	if b, err := os.ReadFile(w.file); err != nil || string(b) != "precious" {
		t.Errorf("data = %q %v", b, err)
	}
	w.onlyClaimedFolders(t)
}

// leftover: a volume of the dev env (on the controller) that has a folder on
// the disk from before the env moved away, and a live plain volume on w1.
func (w *adoptWorld) leftover(t *testing.T) store.Volume {
	t.Helper()
	ctx := context.Background()
	v, _, err := w.orch.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: w.dev.ID}, "left", nil)
	must(t, err)
	must(t, os.MkdirAll(filepath.Join(w.env.Root, "disk", w.row.ID, v.Name), 0o755))
	must(t, w.st.DiskFolders.Create(ctx, store.DiskFolder{ID: "f-left", DiskID: w.row.ID, Name: v.Name, CreatedAt: time.Now()}))
	w.wFake.Edit(func() {
		w.wFake.Volumes = append(w.wFake.Volumes, docker.VolumeInfo{Name: v.Name, SizeBytes: 100})
	})
	return v
}

func (w *adoptWorld) leftoverIntact(t *testing.T, v store.Volume) {
	t.Helper()
	if sawCall(w.wFake, "RemoveVolume("+v.Name) {
		t.Errorf("the live plain volume was removed: %v", callNames(w.wFake))
	}
	if bound, plain := w.createsOf(v.Name); bound+plain != 0 {
		t.Errorf("the leftover was made again: %d %d", bound, plain)
	}
	if slices.Contains(w.folders(t), v.Name) {
		t.Errorf("the leftover's folder row is still there")
	}
	if found, dev, _ := w.wFake.VolumeDevice(context.Background(), v.Name); !found || dev != "" {
		t.Errorf("the plain volume = %v %q", found, dev)
	}
}

// DL: env moved A to B, A died, A's disk is adopted on B.
func TestAdoptWithMoveLeftoverKeepsLivePlainVolume(t *testing.T) {
	w := newAdoptWorld(t)
	v := w.leftover(t) // dev is on the controller: its volume is not the old node's
	w.done(t)
	w.leftoverIntact(t, v)
}

// DL: the env was restored from backup onto B before the adopt: same result.
func TestAdoptAfterRestoreElsewhere(t *testing.T) {
	w := newAdoptWorld(t)
	v := w.leftover(t)
	must(t, w.st.Environments.SetNode(context.Background(), w.dev.ID, "w1"))
	w.done(t)
	w.leftoverIntact(t, v)
	for _, j := range w.jobsOf(t, string(kindDeploy)) {
		if slices.Contains(j.LockSet, w.devT.ID) && !strings.Contains(j.Payload, w.devT.ID) {
			continue
		}
		if strings.Contains(j.Payload, w.devT.ID) {
			t.Errorf("dev env was part of the adopt: %s", j.Payload)
		}
	}
}

func TestAdoptRefusedWhenTheNewNodeHasADisk(t *testing.T) {
	w := newAdoptWorld(t)
	must(t, w.st.NodeDisks.Create(context.Background(), store.NodeDisk{ID: "other", NodeID: "w1", Kind: "path", MountPath: "/other", State: disk.StateReady, CreatedAt: time.Now()}))
	if err := w.pressErr(t); err == nil || !strings.Contains(err.Error(), "already has a disk") {
		t.Fatalf("err = %v", err)
	}
	w.unchanged(t)
}

func TestAdoptListsDiskRowsOfForgottenNodes(t *testing.T) {
	w := newAdoptWorld(t)
	p, err := w.orch.PlanDiskAdopt(context.Background(), "w1", "")
	must(t, err)
	if len(p.Candidates) != 1 || p.Candidates[0] != (AdoptCandidate{DiskID: w.row.ID, Node: "oldbox", Path: "/disk", State: disk.StateReady}) {
		t.Errorf("candidates = %+v", p.Candidates)
	}
	w.done(t)
	if p, err = w.orch.PlanDiskAdopt(context.Background(), "w1", ""); err != nil || len(p.Candidates) != 0 {
		t.Errorf("after the adopt: %+v %v", p.Candidates, err)
	}
}

func (w *adoptWorld) addOldNode(t *testing.T, status string) {
	t.Helper()
	must(t, w.st.Nodes.Create(context.Background(), store.Node{ID: "old", Name: "oldbox", Slug: "oldbox", Role: node.Worker, Tags: "[]",
		RangeCIDR: "10.211.0.0/16", Status: status, CreatedAt: time.Now()}))
}

// DL: an online old node still owns its disk.
func TestAdoptRefusedWhileOldNodeOnline(t *testing.T) {
	w := newAdoptWorld(t)
	w.addOldNode(t, node.StatusOnline)
	err := w.pressErr(t)
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "still online") {
		t.Fatalf("err = %v", err)
	}
	w.unchanged(t)
}

// DL: a folder disk can be mounted by two hosts at once, so offline is not enough.
func TestAdoptPathRefusedWhileOldNodeOnlyOffline(t *testing.T) {
	w := newAdoptWorld(t)
	w.addOldNode(t, node.StatusOffline)
	err := w.pressErr(t)
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "not forgotten") {
		t.Fatalf("err = %v", err)
	}
	w.unchanged(t)
	if p, err := w.orch.PlanDiskAdopt(context.Background(), "w1", "/disk"); err != nil || !strings.Contains(p.Refusal, "not forgotten") {
		t.Errorf("plan refusal = %q %v", p.Refusal, err)
	}
}

func (w *adoptWorld) member(t *testing.T, id, nodeID string) {
	t.Helper()
	ctx := context.Background()
	m, err := w.orch.operated.Create(ctx, w.devT.ID, "postgres", "stackr", "")
	must(t, err)
	v := w.data
	v.InstanceID = &m.ID
	must(t, w.st.Volumes.Update(ctx, v))
	must(t, w.st.OperatedMembers.Create(ctx, store.OperatedMember{ID: id, InstanceID: m.ID, Role: opmember.Replica,
		NodeID: nodeID, Volume: "std-vol", State: opmember.Running, Phase: opmember.Streaming, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
}

// DL: a live standby elsewhere, or a member on the old node, refuses.
func TestAdoptRefusedWithLiveMember(t *testing.T) {
	w := newAdoptWorld(t)
	w.member(t, "m-std", w.ctl.ID)
	if err := w.pressErr(t); err == nil || !strings.Contains(err.Error(), "standby") {
		t.Fatalf("standby elsewhere: %v", err)
	}
	w.unchanged(t)
	must(t, w.st.OperatedMembers.Delete(context.Background(), "m-std"))
	w.member(t, "m-old", "old")
	if err := w.pressErr(t); err == nil || !strings.Contains(err.Error(), "Remove the standbys first") {
		t.Fatalf("member on the old node: %v", err)
	}
	w.unchanged(t)
}

// DL: a folder whose stamp is not this disk's is refused, before any change.
func TestAdoptUnknownStampRefused(t *testing.T) {
	w := newAdoptWorld(t)
	w.setStamp(t, "id", "someone-else")
	err := w.pressErr(t)
	if !errors.Is(err, errs.ErrRefused) {
		t.Fatalf("err = %v", err)
	}
	p, err := w.orch.PlanDiskAdopt(context.Background(), "w1", "/disk")
	must(t, err)
	if p.DiskID != "" || p.Refusal == "" {
		t.Errorf("plan = %+v", p)
	}
	w.setStamp(t, "id", w.row.ID)
	w.unchanged(t)
}

// DL: a plain copy with data of a folder name on the new node blocks the
// adopt before anything is recorded; the plan lists it; discarding it frees
// the adopt, and the folder keeps its data.
func TestAdoptRefusedBeforeStep2WhenAPlainCopyOfAFolderNameIsOnTheNewNode(t *testing.T) {
	ctx := context.Background()
	w := newAdoptWorld(t)
	w.wFake.Edit(func() {
		w.wFake.Volumes = append(w.wFake.Volumes, docker.VolumeInfo{Name: w.data.Name, SizeBytes: 9})
	})
	err := w.pressErr(t)
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "plain copy") {
		t.Fatalf("err = %v", err)
	}
	w.unchanged(t)
	p, err := w.orch.PlanDiskAdopt(ctx, "w1", "/disk")
	must(t, err)
	if len(p.Copies) != 1 || p.Copies[0].VolumeID != w.data.ID || p.Refusal == "" {
		t.Fatalf("plan = %+v", p)
	}
	if err := w.orch.DiscardAdoptCopy(ctx, adminP(), "w1", "/disk", w.row.ID, w.data.ID, "nope"); !isInvalid(err) {
		t.Errorf("wrong confirm = %v", err)
	}
	must(t, w.orch.DiscardAdoptCopy(ctx, adminP(), "w1", "/disk", w.row.ID, w.data.ID, w.data.Slug))
	if !sawCall(w.wFake, "RemoveVolume("+w.data.Name) {
		t.Error("the plain copy was not removed")
	}
	w.wFake.Edit(func() { w.wFake.Volumes = nil })
	w.done(t)
	if b, err := os.ReadFile(w.file); err != nil || string(b) != "precious" {
		t.Errorf("data = %q %v", b, err)
	}
}

// DL: an env with volumes and no tiles has no tile to lock: its volume keys are in the set.
func TestAdoptLockSetHasTheEnvVolumeKeysForAnEnvWithoutTiles(t *testing.T) {
	ctx := context.Background()
	w := newAdoptWorld(t)
	bare, err := w.orch.CreateEnv(ctx, w.stack, "bare", EnvSpec{NodeID: "w1", Type: environment.Static, FromKind: environment.FromBranch, FromBranch: "main"})
	must(t, err)
	must(t, w.st.Environments.SetNode(ctx, bare.ID, "old"))
	bv, _, err := w.orch.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: bare.ID}, "cache", nil)
	must(t, err)
	must(t, w.orch.pushDisks(ctx, store.Node{ID: "w1", Name: "w1"}))
	w.done(t)
	js := w.jobsOf(t, string(kindDiskAdopt))
	if len(js) != 1 {
		t.Fatalf("adopt jobs = %d", len(js))
	}
	for _, k := range []string{"volume:" + bv.ID, "volume:" + w.data.ID, "volume:" + w.legacy.ID, w.tl.ID, "disk:" + w.row.ID, "node:w1"} {
		if !slices.Contains(js[0].LockSet, k) {
			t.Errorf("lock set %v lacks %s", []string(js[0].LockSet), k)
		}
	}
}

// DL: a second press while an adopt job is live is refused and never cancels it.
func TestAdoptSecondPressRefusedWhileAnAdoptJobIsLiveAndNeverCancelsIt(t *testing.T) {
	ctx := context.Background()
	w := newAdoptWorld(t)
	must(t, w.st.Jobs.Create(ctx, store.Job{ID: "live", Kind: string(kindDiskAdopt), State: job.Running,
		LockSet: store.StringList{"disk:" + w.row.ID}, Payload: "{}", CreatedAt: time.Now()}))
	err := w.pressErr(t)
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v", err)
	}
	if j, err := w.st.Jobs.Get(ctx, "live"); err != nil || j.State != job.Running {
		t.Errorf("the live job = %+v %v", j, err)
	}
	w.unchanged(t)
}

// cancelHost cancels the job's context while it answers a claim with a refusal.
type cancelHost struct {
	disk.Host
	cancel context.CancelFunc
}

func (h cancelHost) Disk(ctx context.Context, nodeID string, r hostdisk.Request) (hostdisk.Result, error) {
	if r.Op == "claim" {
		h.cancel()
		return hostdisk.Result{Error: &hostdisk.Error{Code: hostdisk.CodeForeignStamp, Message: "the disk carries another stamp"}}, nil
	}
	return h.Host.Disk(ctx, nodeID, r)
}

// A refused claim wrote nothing, so the row goes back, and that cleanup runs
// although the job's context is cancelled.
func TestAdoptFailureCleanupRunsOnAContextThatIsNotCancelled(t *testing.T) {
	w := newAdoptWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.orch.diskLeaf = disk.New(w.st.NodeDisks, w.st.DiskFolders, cancelHost{InProcessDisk(w.env), cancel})
	err := w.orch.runDiskAdopt(ctx, &jobs.Run{Log: io.Discard}, diskAdoptJob{DiskID: w.row.ID, NodeID: "w1", Path: "/disk"})
	var he *hostdisk.Error
	if !errors.As(err, &he) || he.Code != hostdisk.CodeForeignStamp {
		t.Fatalf("err = %v", err)
	}
	r := w.get(t)
	if r.State != disk.StateMissing || r.OpNodeID != "" || r.OpEnvIDs != "" || r.NodeID != "old" || w.envNode(t) != "old" {
		t.Errorf("row = %+v env %s", r, w.envNode(t))
	}
}

// DL: the restores wait on the adopt's locks (their lock sets overlap it), and
// one that runs on the not-ready row after a crash fails and changes nothing.
func TestAdoptQueuedRestoresWaitOnTheAdoptLocksAndFailOnTheNotReadyRowAfterACrash(t *testing.T) {
	ctx := context.Background()
	w := newAdoptWorld(t)
	w.done(t)
	adopt := w.jobsOf(t, string(kindDiskAdopt))[0]
	rs := w.jobsOf(t, string(kindRestore))
	if len(rs) != 1 || !slices.Contains(rs[0].LockSet, "volume:"+w.legacy.ID) || !slices.Contains(adopt.LockSet, "volume:"+w.legacy.ID) {
		t.Fatalf("restore locks %v, adopt locks %v", rs, []string(adopt.LockSet))
	}
	// crash before the last step: the row is adopting again, a restore that runs fails
	r := w.get(t)
	r.State, r.OpNodeID, r.OpFromNodeID, r.OpMountPath, r.OpEnvIDs = disk.StateAdopting, "w1", "old", "/disk", `["`+w.edge.ID+`"]`
	must(t, w.st.NodeDisks.Update(ctx, r))
	n := len(w.wFake.Created)
	j, err := w.orch.queueRestore(ctx, "run1", w.legacy, w.legacy)
	if j = w.run(t, j, err); j.State != job.Failed {
		t.Errorf("restore on the not-ready row = %s %q", j.State, j.Error)
	}
	if len(w.wFake.Created) != n {
		t.Errorf("volumes created: %+v", w.wFake.Created[n:])
	}
}

// DL: crash after step 2, the row says adopting and nothing else changed.
func TestAdoptCrashAfterAdoptingRowResumes(t *testing.T) {
	w := newAdoptWorld(t)
	d := w.step2(t)
	if d.State != disk.StateAdopting || d.OpNodeID != "w1" || d.OpFromNodeID != "old" || d.OpMountPath != "/disk" || d.OpEnvIDs != `["`+w.edge.ID+`"]` {
		t.Fatalf("row = %+v", d)
	}
	if got, ok, _ := w.orch.nodeDisk(context.Background(), "w1"); !ok || got.ID != w.row.ID { // the new node already sees it, not ready
		t.Errorf("w1 sees %+v %v", got, ok)
	}
	if w.envNode(t) != "old" || w.stampOwner(t) != "old" {
		t.Error("something besides the row changed")
	}
	w.done(t)
	if r := w.get(t); r.State != disk.StateReady || r.NodeID != "w1" {
		t.Errorf("row = %+v", r)
	}
}

// DL: crash after the claim: the same call is accepted because the owner is
// already the new node.
func TestAdoptCrashAfterClaimResumesAndClaimAcceptsTheNewOwner(t *testing.T) {
	w := newAdoptWorld(t)
	d := w.step2(t)
	w.step3(t, d)
	if w.stampOwner(t) != "w1" {
		t.Fatal("claim did not write")
	}
	w.step3(t, d) // idempotent
	w.done(t)
	if r := w.get(t); r.State != disk.StateReady || r.NodeID != "w1" || w.envNode(t) != "w1" {
		t.Errorf("row = %+v env %s", r, w.envNode(t))
	}
}

// DL: before the commit nothing but the row and the stamp changed; the re-run derives the same claim set.
func TestAdoptCrashBeforeTransactionCommitChangesNothing(t *testing.T) {
	w := newAdoptWorld(t)
	must(t, w.st.DiskFolders.Create(context.Background(), store.DiskFolder{ID: "f-stray", DiskID: w.row.ID, Name: "stray", CreatedAt: time.Now()}))
	before := w.folders(t)
	d := w.step2(t)
	w.step3(t, d)
	if got := w.get(t); got.NodeID != "old" || w.envNode(t) != "old" || !slices.Equal(w.folders(t), before) {
		t.Fatalf("row %+v env %s folders %v", got, w.envNode(t), w.folders(t))
	}
	w.done(t)
	w.onlyClaimedFolders(t)
}

// DL: after the commit no later step needs the claim set; the re-run skips to step 5.
func TestAdoptCrashAfterTransactionResumesWithoutTheClaimSet(t *testing.T) {
	w := newAdoptWorld(t)
	d := w.step2(t)
	w.step3(t, d)
	d = w.step4(t, d)
	if d.NodeID != "w1" || d.State != disk.StateAdopting || w.envNode(t) != "w1" {
		t.Fatalf("row %+v env %s", d, w.envNode(t))
	}
	w.done(t)
	if r := w.get(t); r.State != disk.StateReady || r.OpNodeID != "" {
		t.Errorf("row = %+v", r)
	}
	if bound, _ := w.createsOf(w.data.Name); bound == 0 {
		t.Error("the data volume was not made")
	}
}

func TestAdoptCrashAfterEnsureResumes(t *testing.T) {
	w := newAdoptWorld(t)
	d := w.step2(t)
	w.step3(t, d)
	d = w.step4(t, d)
	ctx := disk.WithOp(context.Background(), d.ID, disk.StateAdopting)
	_, err := w.orch.volumes.Ensure(ctx, w.data)
	must(t, err)
	w.done(t)
	if r := w.get(t); r.State != disk.StateReady {
		t.Errorf("row = %+v", r)
	}
}

// DL: the row goes ready only after every env is queued: a failing restore
// keeps it adopting, the next press queues them again and finishes.
func TestAdoptCrashAfterRestoresQueuedStaysAdoptingAndResumes(t *testing.T) {
	w := newAdoptWorld(t)
	w.wFake.Err = map[string]error{"CreateVolume(" + w.legacy.Name + ")": errors.New("daemon busy")}
	if j := w.press(t); j.State != job.Failed {
		t.Fatalf("job = %s, want failed", j.State)
	}
	if r := w.get(t); r.State != disk.StateAdopting || r.NodeID != "w1" || r.OpEnvIDs == "" {
		t.Fatalf("after the failure: %+v", r)
	}
	w.wFake.Edit(func() { w.wFake.Err = nil })
	w.done(t)
	if r := w.get(t); r.State != disk.StateReady || r.OpNodeID != "" || r.OpEnvIDs != "" {
		t.Errorf("row = %+v", r)
	}
	if len(w.jobsOf(t, string(kindRestore))) == 0 {
		t.Error("no restore queued")
	}
}

// DL: ready only after every env is queued; every resume uses the persisted env set.
func TestAdoptStateReadyOnlyAfterEveryEnvIsQueued(t *testing.T) {
	ctx := context.Background()
	w := newAdoptWorld(t)
	w.stranded(t)
	// an env that came to w1 after the operation was recorded is not part of it
	must(t, w.st.Environments.SetNode(ctx, w.dev.ID, "w1"))
	if r := w.get(t); r.State != disk.StateAdopting {
		t.Fatalf("state = %s", r.State)
	}
	w.done(t)
	for _, j := range w.jobsOf(t, string(kindDeploy)) {
		if strings.Contains(j.Payload, w.devT.ID) {
			t.Errorf("dev env was deployed by the adopt: %s", j.Payload)
		}
	}
}

// DL: an adopting row refuses another node and another path.
func TestAdoptSecondPressForAnotherNodeIsRefusedWhileAdopting(t *testing.T) {
	ctx := context.Background()
	w := newAdoptWorld(t)
	w.step2(t)
	if _, err := w.orch.AdoptDisk(ctx, adminP(), w.ctl.ID, w.row.ID, "/disk", w.ctl.Name); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("another node = %v", err)
	}
	if _, err := w.orch.AdoptDisk(ctx, adminP(), "w1", w.row.ID, "/elsewhere", "w1"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("another path = %v", err)
	}
	if r := w.get(t); r.OpNodeID != "w1" || r.OpMountPath != "/disk" {
		t.Errorf("the target changed: %+v", r)
	}
}

func TestAdoptNeedsTheTypedConfirmAndAdmin(t *testing.T) {
	ctx := context.Background()
	w := newAdoptWorld(t)
	if _, err := w.orch.AdoptDisk(ctx, adminP(), "w1", w.row.ID, "/disk", "nope"); !isInvalid(err) {
		t.Errorf("wrong confirm = %v", err)
	}
	if _, err := w.orch.AdoptDisk(ctx, ownerP("x"), "w1", w.row.ID, "/disk", "w1"); err == nil {
		t.Error("a non-admin adopted a disk")
	}
	w.unchanged(t)
}

// An orphan pinned to the dead node, with a folder, is carried over.
func TestAdoptCarriesOrphansOfTheOldNode(t *testing.T) {
	ctx := context.Background()
	w := newAdoptWorld(t)
	now := time.Now()
	orph := store.Volume{ID: "orph", ScopeKind: "env", ScopeID: "gone-env", Slug: "old-cache", Name: "orph-vol", OrphanedAt: &now, NodeID: "old", CreatedAt: now}
	must(t, w.st.Volumes.Create(ctx, orph))
	must(t, os.MkdirAll(filepath.Join(w.env.Root, "disk", w.row.ID, "orph-vol"), 0o755))
	must(t, w.st.DiskFolders.Create(ctx, store.DiskFolder{ID: "f-orph", DiskID: w.row.ID, Name: "orph-vol", CreatedAt: now}))
	w.done(t)
	got, err := w.st.Volumes.Get(ctx, "orph")
	must(t, err)
	if got.NodeID != "w1" || !slices.Contains(w.folders(t), "orph-vol") {
		t.Errorf("orphan on %q, folders %v", got.NodeID, w.folders(t))
	}
	if bound, _ := w.createsOf("orph-vol"); bound == 0 {
		t.Error("the orphan's object was not made")
	}
}
