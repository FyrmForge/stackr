package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

var opStates = []string{disk.StateAdopting, disk.StateForgetting}

// DL: a disk in adopting or forgetting refuses every verb of the volume leaf
// and every deploy, and creates nothing.
func TestStatesAdoptingAndForgettingAreNotReadyForEveryVerb(t *testing.T) {
	ctx := context.Background()
	for _, state := range opStates {
		t.Run(state, func(t *testing.T) {
			d := newDiskTest(t)
			row := d.addDisk(t)
			v := d.vol(t, "data")
			tl := d.diskTile(t, "db", "data:/data")
			setState(t, d, row, state)
			a := diskAdapter{d.orch}
			if st, err := a.State(ctx, "w1"); err != nil || st != volume.DiskNotReady {
				t.Errorf("State = %v, %v", st, err)
			}
			var vm *volume.MissingError
			if _, err := a.Place(ctx, "w1", v.Name); !errors.As(err, &vm) {
				t.Errorf("Place err = %v", err)
			}
			if _, err := a.Usage(ctx, "w1", volume.UseFresh, v.Name); !errors.As(err, &vm) {
				t.Errorf("Usage err = %v", err)
			}
			if err := a.Drop(ctx, "w1", v.Name); !errors.As(err, &vm) {
				t.Errorf("Drop err = %v", err)
			}
			if _, err := d.orch.volumes.Ensure(ctx, v); err == nil {
				t.Error("Ensure went on")
			}
			var dm *disk.MissingError
			if _, _, err := d.orch.dataDiskOf(ctx, "w1"); !errors.As(err, &dm) {
				t.Errorf("dataDiskOf err = %v", err)
			}
			j, err := d.orch.Deploy(ctx, tl.ID)
			switch {
			case err != nil:
			default:
				if j = d.wait(t, j); j.State != job.Failed {
					t.Errorf("deploy: %s %q", j.State, j.Error)
				}
			}
			if n := volumeCreates(d.wFake); n != 0 {
				t.Errorf("%d volumes created: %v", n, callNames(d.wFake))
			}
		})
	}
}

// DL: the context opens the disk only for its own id and the state it names.
func TestOpContextIsHonouredOnlyForItsOwnDiskAndState(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	a := diskAdapter{d.orch}
	state := func(c context.Context) volume.DiskState {
		t.Helper()
		st, err := a.State(c, "w1")
		must(t, err)
		return st
	}
	setState(t, d, row, disk.StateAdopting)
	for _, c := range []struct {
		name string
		ctx  context.Context
		want volume.DiskState
	}{
		{"none", ctx, volume.DiskNotReady},
		{"own adopt", disk.WithOp(ctx, row.ID, disk.StateAdopting), volume.DiskReady},
		{"other disk", disk.WithOp(ctx, "other", disk.StateAdopting), volume.DiskNotReady},
		{"forget op on adopting row", disk.WithOp(ctx, row.ID, disk.StateForgetting), volume.DiskNotReady},
		{"dropped", disk.WithoutOp(disk.WithOp(ctx, row.ID, disk.StateAdopting)), volume.DiskNotReady},
	} {
		if got := state(c.ctx); got != c.want {
			t.Errorf("adopting row, %s: %v, want %v", c.name, got, c.want)
		}
	}
	setState(t, d, row, disk.StateForgetting)
	for _, c := range []struct {
		name string
		ctx  context.Context
		want volume.DiskState
	}{
		{"none", ctx, volume.DiskNotReady},
		{"own forget", disk.WithOp(ctx, row.ID, disk.StateForgetting), volume.DiskNone},
		{"other disk", disk.WithOp(ctx, "other", disk.StateForgetting), volume.DiskNotReady},
		{"adopt op on forgetting row", disk.WithOp(ctx, row.ID, disk.StateAdopting), volume.DiskNotReady},
	} {
		if got := state(c.ctx); got != c.want {
			t.Errorf("forgetting row, %s: %v, want %v", c.name, got, c.want)
		}
	}
	// a ready row needs no op, and an op changes nothing for it
	setState(t, d, row, disk.StateReady)
	if got := state(disk.WithOp(ctx, row.ID, disk.StateForgetting)); got != volume.DiskReady {
		t.Errorf("ready row under a forget op = %v", got)
	}
	// under an adopt the volume leaf can place a folder
	setState(t, d, row, disk.StateAdopting)
	v := d.vol(t, "data")
	if _, err := d.orch.volumes.Ensure(disk.WithOp(ctx, row.ID, disk.StateAdopting), v); err != nil {
		t.Errorf("Ensure under the adopt: %v", err)
	}
}

// DL: a job handler never inherits an operation context, so a job queued
// from inside an adopt or forget runs under the ordinary rules.
func TestOpContextIsNotCarriedIntoAQueuedJob(t *testing.T) {
	var seen string
	inner := func(ctx context.Context, _ *jobs.Run) error {
		seen = opMode(ctx, store.NodeDisk{ID: "d1", State: disk.StateAdopting})
		return nil
	}
	op := disk.WithOp(context.Background(), "d1", disk.StateAdopting)
	if inner(op, nil); seen != disk.StateAdopting {
		t.Fatalf("the check itself is blind: %q", seen)
	}
	if err := ownedJob(inner)(op, nil); err != nil || seen != "" {
		t.Errorf("handler under ownedJob saw %q, %v", seen, err)
	}
	// the three real kinds are wrapped the same way, and a queued one runs
	// with no op whatever the enqueuing context held
	d := newDiskTest(t)
	row := d.addDisk(t)
	setState(t, d, row, disk.StateAdopting)
	j, err := d.orch.enqueue(disk.WithOp(context.Background(), row.ID, disk.StateAdopting), kindDiskForget, diskForgetJob{DiskID: row.ID})
	must(t, err)
	if j = d.wait(t, j); j.State != job.Failed {
		t.Errorf("job = %s %q", j.State, j.Error)
	}
	if got, _, _ := d.orch.diskInOp(context.Background(), row.ID); got.State != disk.StateAdopting {
		t.Errorf("row = %+v", got)
	}
}

// DL: dataDiskOf follows the context the way the leaf's state does.
func TestDataDiskHonoursTheOpContextAdoptingGivesTheNodeDiskForgettingTheDockerDisk(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	d.facts(t, worker.Facts{DisksRead: true, Disks: []hostdisk.Fact{{Path: "/disk", Free: 7, Total: 10}}})
	var dm *disk.MissingError

	setState(t, d, row, disk.StateAdopting)
	if _, _, err := d.orch.dataDiskOf(ctx, "w1"); !errors.As(err, &dm) {
		t.Errorf("adopting, no op: %v", err)
	}
	if f, tot, err := d.orch.dataDiskOf(disk.WithOp(ctx, row.ID, disk.StateAdopting), "w1"); err != nil || f != 7 || tot != 10 {
		t.Errorf("adopting under its op = %d, %d, %v; want the node disk", f, tot, err)
	}
	if _, _, err := d.orch.dataDiskOf(disk.WithOp(ctx, row.ID, disk.StateForgetting), "w1"); !errors.As(err, &dm) {
		t.Errorf("adopting under a forget op: %v", err)
	}

	setState(t, d, row, disk.StateForgetting)
	if _, _, err := d.orch.dataDiskOf(ctx, "w1"); !errors.As(err, &dm) {
		t.Errorf("forgetting, no op: %v", err)
	}
	if _, _, err := d.orch.dataDiskOf(disk.WithOp(ctx, row.ID, disk.StateAdopting), "w1"); !errors.As(err, &dm) {
		t.Errorf("forgetting under an adopt op: %v", err)
	}
	wantF, wantT := d.orch.diskOf(ctx, "w1")
	f, tot, err := d.orch.dataDiskOf(disk.WithOp(ctx, row.ID, disk.StateForgetting), "w1")
	if err != nil || f != wantF || tot != wantT || (f == 7 && tot == 10) {
		t.Errorf("forgetting under its op = %d, %d, %v; want the Docker disk %d, %d", f, tot, err, wantF, wantT)
	}
	// another disk's op does not open this one
	if _, _, err := d.orch.dataDiskOf(disk.WithOp(ctx, "other", disk.StateForgetting), "w1"); !errors.As(err, &dm) {
		t.Errorf("another disk's op: %v", err)
	}
}

// An adopting row also stands for its target node: its capacity comes from
// the path on that node.
func TestDataDiskOfAnAdoptingRowReadsTheTargetNodesPath(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t) // w1's, as if it were the old node
	must(t, d.st.Nodes.Create(ctx, store.Node{ID: "w2", Name: "w2", Slug: "w2", Role: node.Worker, Tags: "[]", RangeCIDR: "10.210.0.0/16",
		Status: node.StatusOnline, CreatedAt: time.Now()}))
	row.State, row.OpNodeID, row.OpFromNodeID, row.OpMountPath = disk.StateAdopting, "w2", "w1", "/new"
	row.NodeID = "w1"
	must(t, d.st.NodeDisks.Update(ctx, row))
	d.orch.mesh.mu.Lock()
	d.orch.mesh.facts["w2"] = worker.Facts{DisksRead: true, Disks: []hostdisk.Fact{{Path: "/new", Free: 3, Total: 9}}}
	d.orch.mesh.at["w2"] = time.Now()
	d.orch.mesh.mu.Unlock()
	if f, tot, err := d.orch.dataDiskOf(disk.WithOp(ctx, row.ID, disk.StateAdopting), "w2"); err != nil || f != 3 || tot != 9 {
		t.Errorf("w2 = %d, %d, %v", f, tot, err)
	}
	if _, _, err := d.orch.dataDiskOf(ctx, "w2"); err == nil {
		t.Error("w2 saw its disk as ready without the op")
	}
}

func TestDiskInOpAndOpEnvIDs(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	if _, ok, err := d.orch.diskInOp(ctx, row.ID); err != nil || ok {
		t.Errorf("ready row: ok=%v err=%v", ok, err)
	}
	row.State, row.OpEnvIDs = disk.StateAdopting, `["e1","e2"]`
	must(t, d.st.NodeDisks.Update(ctx, row))
	got, ok, err := d.orch.diskInOp(ctx, row.ID)
	if err != nil || !ok {
		t.Fatalf("adopting row: ok=%v err=%v", ok, err)
	}
	if ids, err := opEnvIDs(got); err != nil || len(ids) != 2 || ids[1] != "e2" {
		t.Errorf("env ids = %v, %v", ids, err)
	}
	if ids, err := opEnvIDs(store.NodeDisk{}); err != nil || ids != nil {
		t.Errorf("no ids = %v, %v", ids, err)
	}
	if _, _, err := d.orch.diskInOp(ctx, "nope"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown row err = %v", err)
	}
}

// The three job kinds and the verbs of the later packages are registered and
// refuse until they are built.
func TestPhase2StubsRefuseAndAreRegistered(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	hs := d.orch.handlers()
	for _, k := range []jobs.Kind{kindDiskAdopt, kindDiskForget, kindVolumeToDisk} {
		if hs[k] == nil {
			t.Errorf("%s has no handler", k)
			continue
		}
		j, err := d.orch.enqueue(ctx, k, map[string]string{})
		must(t, err)
		if j = d.wait(t, j); j.State != job.Failed || !strings.Contains(j.Error, "Not available yet") {
			t.Errorf("%s ran as %s %q", k, j.State, j.Error)
		}
	}
	o, by := d.orch, adminP()
	refused := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, errs.ErrRefused) {
			t.Errorf("%s err = %v, want a refusal", name, err)
		}
	}
	_, err := o.PlanDiskAdopt(ctx, "w1", "/x")
	refused("PlanDiskAdopt", err)
	_, err = o.AdoptDisk(ctx, by, "w1", "d", "/x", "")
	refused("AdoptDisk", err)
	_, err = o.ForgetNodeDisk(ctx, by, "d", "")
	refused("ForgetNodeDisk", err)
	refused("DiscardPlainCopy", o.DiscardPlainCopy(ctx, by, "v", ""))
	refused("DiscardBoundObject", o.DiscardBoundObject(ctx, by, "v", ""))
	_, err = o.MoveVolumeToDisk(ctx, by, "v")
	refused("MoveVolumeToDisk", err)
	_, err = o.PlanMoveAllToDisk(ctx, by, "w1")
	refused("PlanMoveAllToDisk", err)
	_, err = o.MoveAllToDisk(ctx, by, "w1")
	refused("MoveAllToDisk", err)
}

func TestNewJobKindsAreInDataCaps(t *testing.T) {
	for _, k := range []jobs.Kind{kindDiskAdopt, kindDiskForget, kindVolumeToDisk} {
		if dataCaps[k] != 12*time.Hour {
			t.Errorf("dataCaps[%s] = %v", k, dataCaps[k])
		}
	}
}
