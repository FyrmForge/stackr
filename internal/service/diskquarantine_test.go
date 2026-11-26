package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// quarantine jobs of the rig, newest last.
func (d *diskTest) quarJobs(t *testing.T) []store.Job {
	t.Helper()
	rs, err := d.st.Jobs.ListByState(context.Background(), job.Queued, job.Running, job.Done, job.Failed, job.Waiting)
	must(t, err)
	return slices.DeleteFunc(rs, func(j store.Job) bool { return j.Kind != string(kindDiskQuarantine) })
}

// quarantine latches the disk (as the watch would), runs diskWatch's enqueue
// and waits for the job.
func (d *diskTest) quarantine(t *testing.T, row store.NodeDisk) Job {
	t.Helper()
	ctx := context.Background()
	setState(t, d, row, disk.StateMissing)
	n, err := d.st.Nodes.Get(ctx, "w1")
	must(t, err)
	row.State = disk.StateMissing
	d.orch.quarantineEnqueue(ctx, n, row)
	js := d.quarJobs(t)
	if len(js) == 0 {
		t.Fatal("no disk-quarantine job was queued")
	}
	return d.wait(t, Job{ID: js[len(js)-1].ID})
}

func ioRead(row store.NodeDisk) worker.Facts {
	f := readOf(row, 50*gig, 100*gig)
	f.Disks[0].IOErr = true
	return f
}

// Missing stops the running tile on the disk with no stop job of its own, so
// Check disk redeploys it; a second enqueue finds the finished job.
func TestQuarantineStopsTilesAndCheckDiskRedeploysThem(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, true, false)
	w.wFake.StopExits = map[string]bool{w.cid: true}
	j := w.quarantine(t, w.row)
	if j.State != job.Done {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	if !sawCall(w.wFake, "Stop("+w.cid) {
		t.Errorf("the tile was not stopped: %v", callNames(w.wFake))
	}
	if stopped, err := w.orch.stoppedByUser(ctx, w.tl); err != nil || stopped {
		t.Errorf("stoppedByUser = %v, %v: a quarantine stop is not the user's", stopped, err)
	}
	if got, _, err := w.orch.jobRows.Last(ctx, w.tl.ID); err != nil || got.Kind == string(kindStop) {
		t.Errorf("the tile got a stop job: %+v, %v", got, err)
	}
	n, _ := w.st.Nodes.Get(ctx, "w1")
	row, _, _ := w.orch.nodeDisk(ctx, "w1")
	w.orch.quarantineEnqueue(ctx, n, row)
	if len(w.quarJobs(t)) != 1 {
		t.Errorf("a finished quarantine was queued again")
	}
	w.check(t)
	if !w.deployJobs(t, w.tl) {
		t.Error("Check disk did not redeploy the stopped tile")
	}
	if _, ok, _ := w.orch.quarLoad(ctx, w.row.ID); ok {
		t.Error("the record outlived Check disk")
	}
}

// The kind is in neither list that reads a stop as the user's wish: with a
// finished quarantine newer than the deploy, the tile still counts as run.
func TestQuarantineKindIsNotAUserStop(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, true, false)
	w.wFake.Containers = nil
	now := time.Now()
	for i, k := range []jobs.Kind{kindDeploy, kindDiskQuarantine} {
		must(t, w.st.Jobs.Create(ctx, store.Job{ID: "q" + string(rune('a'+i)), Kind: string(k), State: job.Done, LockSet: store.StringList{w.tl.ID},
			Payload: "{}", CreatedAt: now.Add(time.Duration(i) * time.Minute)}))
	}
	if stopped, err := w.orch.stoppedByUser(ctx, w.tl); err != nil || stopped {
		t.Errorf("stoppedByUser = %v, %v", stopped, err)
	}
	if ran, err := w.orch.ranBeforeRestore(ctx, w.tl); err != nil || !ran {
		t.Errorf("ranBeforeRestore = %v, %v: the quarantine hid the deploy", ran, err)
	}
}

// diskWatch queues the job for a missing row, once; with no finished record
// (a panel restart before the stop) it queues again.
func TestQuarantineIsQueuedByTheWatchAndAgainAfterARestart(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, true, false)
	w.lose(t)
	n, _ := w.st.Nodes.Get(ctx, "w1")
	row, _, _ := w.orch.nodeDisk(ctx, "w1")
	f := readOf(row, 0, 0)
	f.Disks[0].ReadAt = time.Unix(1e9, 0)
	w.orch.diskWatch(ctx, n, f)
	w.orch.diskWatch(ctx, n, f)
	js := w.quarJobs(t)
	if len(js) != 1 {
		t.Fatalf("jobs = %d, want 1 (the second pass finds the first)", len(js))
	}
	w.wait(t, Job{ID: js[0].ID})
	// a panel that died before the record was complete: no finished row
	must(t, w.st.Settings.Delete(ctx, quarKey(row.ID)))
	w.orch.diskWatch(ctx, n, f)
	if len(w.quarJobs(t)) != 2 {
		t.Errorf("jobs = %d, want a new one after a restart with no record", len(w.quarJobs(t)))
	}
}

// Erroring: three fresh bad reads, the alert, the badge; nothing is stopped,
// a stateless env still deploys, an env with a volume on the disk is refused.
func TestErroringBlocksOnlyWorkOnTheDisk(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	h := newHook(t)
	alertsTo(t, d.orch, h, "")
	if err := d.orch.gateEnv(ctx, d.edge.ID, true); err != nil {
		t.Fatalf("healthy gate: %v", err)
	}
	for i := 1; i <= diskMissingReads; i++ {
		if d.orch.DiskErroring("w1") {
			t.Fatalf("erroring after %d reads", i-1)
		}
		d.watch(t, ioRead(row))
	}
	if !d.orch.DiskErroring("w1") {
		t.Fatal("not erroring after the run")
	}
	if got := h.events(t, d.orch); len(got) != 1 || got[0] != AlertDiskMissing+":"+SevCritical {
		t.Errorf("alerts = %v", got)
	}
	if d.stateOf(t, row) != disk.StateReady || len(d.quarJobs(t)) != 0 {
		t.Errorf("an erroring disk was latched or quarantined: %s, %d jobs", d.stateOf(t, row), len(d.quarJobs(t)))
	}
	// the env keeps no data on the disk and mounts none: it passes
	if err := d.orch.gateEnv(ctx, d.edge.ID, true); err != nil {
		t.Errorf("a stateless deploy was refused: %v", err)
	}
	// a declared volume with no folder and no Docker object would be created on the disk
	v := d.vol(t, "data")
	d.diskTile(t, "db", "data:/data")
	if err := d.orch.gateEnv(ctx, d.edge.ID, true); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("a deploy that would create a volume: %v, want refused", err)
	}
	d.ensure(t, v)
	if err := d.orch.gateEnv(ctx, d.edge.ID, true); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("a deploy with a volume on the disk: %v, want refused", err)
	}
	if err := d.orch.gateEnv(ctx, d.edge.ID, false); err != nil {
		t.Errorf("a verb that is no deploy was refused: %v", err)
	}
	// a good read ends it
	d.watch(t, readOf(row, 50*gig, 100*gig))
	if d.orch.DiskErroring("w1") {
		t.Error("still erroring after a good read")
	}
}

// A dying disk's stale reads do not clear the flag: it stays until a good read.
func TestErroringSurvivesAStaleRead(t *testing.T) {
	d := newDiskTest(t)
	row := d.addDisk(t)
	for range diskMissingReads {
		d.watch(t, ioRead(row))
	}
	stale := ioRead(row)
	stale.DisksStale = true
	d.watch(t, stale)
	d.watch(t, ioRead(row))
	if !d.orch.DiskErroring("w1") {
		t.Error("one stale read and one bad read cleared the erroring flag")
	}
	d.watch(t, readOf(row, 50*gig, 100*gig))
	if d.orch.DiskErroring("w1") {
		t.Error("a good read did not clear it")
	}
}

// A failed quarantine is not queued again on every read.
func TestFailedQuarantineIsNotQueuedOnEveryRead(t *testing.T) {
	ctx := context.Background()
	w := newOpWorld(t)
	w.r.reloadBroken = true
	w.r.syncLive = `ANY 1 ("` + mflow.SlotName(w.rep.ID) + `")`
	if j := w.quarantine(t, w.row); j.State != job.Failed {
		t.Fatalf("job = %s, want failed", j.State)
	}
	n, _ := w.st.Nodes.Get(ctx, "w1")
	row, _, _ := w.orch.nodeDisk(ctx, "w1")
	w.orch.quarantineEnqueue(ctx, n, row)
	if got := len(w.quarJobs(t)); got != 1 {
		t.Errorf("jobs = %d, want 1: the failed one is not retried at once", got)
	}
}

// A read-only mount counts like an I/O error; stale reads and runs shorter
// than diskMissingReads do not.
func TestErroringNeedsConsecutiveFreshReads(t *testing.T) {
	d := newDiskTest(t)
	row := d.addDisk(t)
	ro := readOf(row, 50*gig, 100*gig)
	ro.Disks[0].ReadOnly = true
	stale := ro
	stale.DisksStale = true
	d.watch(t, ro)
	d.watch(t, ro)
	d.watch(t, stale) // ends the run
	d.watch(t, ro)
	d.watch(t, ro)
	if d.orch.DiskErroring("w1") {
		t.Fatal("erroring on two plus two reads split by a stale one")
	}
	d.watch(t, ro)
	if !d.orch.DiskErroring("w1") {
		t.Error("three read-only reads in a row are not erroring")
	}
	// a read that carries no error never counts: ENOSPC and quota reads reach
	// the controller as plain reads
	d.watch(t, readOf(row, 0, 100*gig))
	if d.orch.DiskErroring("w1") {
		t.Error("a full disk reads as erroring")
	}
}

// Check disk ends an erroring spell.
func TestCheckDiskClearsErroring(t *testing.T) {
	d := newDiskTest(t)
	row := d.addDisk(t)
	for range diskMissingReads {
		d.watch(t, ioRead(row))
	}
	if !d.orch.DiskErroring("w1") {
		t.Fatal("setup: not erroring")
	}
	d.check(t)
	if d.orch.DiskErroring("w1") {
		t.Error("Check disk left it erroring")
	}
}

// A missing disk refuses the same work.
func TestMissingDiskRefusesWorkOnItButNotAStatelessDeploy(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	setState(t, d, row, disk.StateMissing)
	if err := d.orch.gateEnv(ctx, d.edge.ID, true); err != nil {
		t.Errorf("a stateless deploy was refused: %v", err)
	}
	d.vol(t, "data")
	d.diskTile(t, "db", "data:/data")
	if err := d.orch.gateEnv(ctx, d.edge.ID, true); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("err = %v, want refused", err)
	}
}

// The Nodes list: the hostdisk fact's flags reach the watch (worker facts are
// JSON, so the fields have to survive the trip).
func TestFactFlagsAreJSON(t *testing.T) {
	b, err := json.Marshal(hostdisk.Fact{IOErr: true, ReadOnly: true})
	must(t, err)
	if !strings.Contains(string(b), `"io_err":true`) || !strings.Contains(string(b), `"read_only":true`) {
		t.Errorf("fact = %s", b)
	}
}

// ---- operated members ----

// opWorld is a postgres on the controller with a standby on w1 whose data
// folder is on w1's disk, plus an ordinary tile with a volume on it.
type opWorld struct {
	*diskTest
	r   *failRig
	row store.NodeDisk
	rep InstanceMember
	db  Tile
}

func newOpWorld(t *testing.T) *opWorld {
	t.Helper()
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	r := newFailRigOn(t, d.twoNodes, false)
	_, rep := r.members()
	if rep.ID == "" {
		rep = r.addStandby(t)
	}
	rep, err := d.orch.members.SetSync(ctx, rep, SyncAlways, true)
	must(t, err)
	inst, err := d.orch.operated.SetSync(ctx, r.inst, true, 10, false)
	must(t, err)
	r.inst = inst
	must(t, d.orch.syncInstance(ctx, r.tile(), inst))
	if !r.saw(`ANY 1 ("` + mflow.SlotName(rep.ID) + `")`) {
		t.Fatalf("setup: the sync pass did not name the standby; log:\n%s", strings.Join(r.events, "\n"))
	}
	v := d.vol(t, "data")
	d.ensure(t, v)
	db := d.diskTile(t, "db", "data:/data")
	r.sims["w1"].cs = append(r.sims["w1"].cs, simCtr{Container: docker.Container{ID: "c-db", Name: "db", State: "running",
		Labels: map[string]string{tile.LabelTile: db.ID, tile.LabelRole: "replica"}}})
	return &opWorld{diskTest: d, r: r, row: row, rep: rep, db: db}
}

func (r *failRig) addStandby(t *testing.T) InstanceMember {
	t.Helper()
	m, err := r.n.orch.AddStandby(context.Background(), adminP(), r.inst.TileID, "w1")
	must(t, err)
	return m
}

func (w *opWorld) member(t *testing.T, id string) InstanceMember {
	t.Helper()
	m, err := w.orch.members.Get(context.Background(), id)
	must(t, err)
	return m
}

// Mixed: the ordinary tile and the standby stop, the standby leaves the sync
// list first, its row keeps every field, the primary is not touched; Check
// disk starts exactly those two.
func TestQuarantineMixedStopsStandbyAfterTheSyncDropAndKeepsItsRow(t *testing.T) {
	ctx := context.Background()
	w := newOpWorld(t)
	before := w.member(t, w.rep.ID)
	j := w.quarantine(t, w.row)
	if j.State != job.Done {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	w.r.order("stackr-sync.conf", "stop "+w.rep.Container)
	if strings.Contains(w.r.syncFile, "synchronous_standby_names = '") {
		t.Errorf("the sync file still names a standby: %q", w.r.syncFile)
	}
	if w.r.saw("stop pg") {
		t.Error("the primary was stopped")
	}
	// the pass takes it out of the sync list, and with it its sync_in flag; what
	// the person or the topology owns stays
	after := w.member(t, w.rep.ID)
	if after.SyncIn {
		t.Error("sync_in is still set for a standby the primary no longer waits for")
	}
	if after.State != before.State || after.Fenced != before.Fenced || after.Role != before.Role || after.Phase != before.Phase ||
		after.Routed != before.Routed || after.SyncMode != before.SyncMode || after.NodeID != before.NodeID || after.Volume != before.Volume {
		t.Errorf("the standby row changed:\n before %+v\n after  %+v", before, after)
	}
	// and the sync pass that runs next does not put it back
	inst, err := w.orch.operated.Get(ctx, w.r.inst.ID)
	must(t, err)
	must(t, w.orch.syncInstance(ctx, w.r.tile(), inst))
	if strings.Contains(w.r.syncFile, "synchronous_standby_names = '") {
		t.Errorf("the next sync pass named the stopped standby again: %q", w.r.syncFile)
	}
	rec, ok, _ := w.orch.quarLoad(ctx, w.row.ID)
	if !ok || !rec.Done || !slices.Equal(rec.Members, []string{w.rep.ID}) || !slices.Equal(rec.Tiles, []string{w.db.ID}) {
		t.Errorf("record = %+v", rec)
	}
	// the tick does not bring the standby back before Check disk
	w.r.mu.Lock()
	w.r.events = nil
	w.r.mu.Unlock()
	w.orch.startMembers(ctx)
	if w.r.saw("start " + w.rep.Container) {
		t.Error("startMembers started a standby on a missing disk")
	}
	w.check(t)
	if !w.r.saw("start " + w.rep.Container) {
		t.Errorf("Check disk did not start the standby: %v", w.r.events)
	}
	if !w.deployJobs(t, w.db) {
		t.Error("Check disk did not redeploy the ordinary tile")
	}
}

// A primary that failed over onto the disk is left to failover.
func TestQuarantineLeavesAFailedOverPrimaryAlone(t *testing.T) {
	ctx := context.Background()
	w := newOpWorld(t)
	// the standby was promoted: its row is now the instance's primary, on w1, on the disk
	old, _ := w.r.members()
	must(t, w.st.OperatedMembers.Delete(ctx, old.ID))
	fo := w.member(t, w.rep.ID)
	fo.Role = opmember.Primary
	must(t, w.st.OperatedMembers.Update(ctx, fo))
	j := w.quarantine(t, w.row)
	if j.State != job.Done {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	if w.r.saw("stop " + w.rep.Container) {
		t.Error("a primary was stopped")
	}
	if rec, _, _ := w.orch.quarLoad(ctx, w.row.ID); slices.Contains(rec.Members, w.rep.ID) {
		t.Errorf("record = %+v", rec)
	}
	if !strings.Contains(jobLog(t, w.diskTest, j), "member "+w.rep.ID+" is a primary") {
		t.Errorf("log = %q", jobLog(t, w.diskTest, j))
	}
}

// A standalone operated primary (no standby), with its archive volume on the
// disk, is stopped like a tile, recorded, kept out of the tick and resumed by
// Check disk. A primary that has a standby stays up: it is left to failover.
func TestQuarantineStopsAStandaloneOperatedPrimaryButNotOneWithAStandby(t *testing.T) {
	for _, withStandby := range []bool{false, true} {
		ctx := context.Background()
		w := newForgetWorld(t, true, false)
		w.wFake.StopExits = map[string]bool{w.cid: true}
		m, err := w.orch.operated.Create(ctx, w.tl.ID, "postgres", "stackr", "")
		must(t, err)
		_, err = w.orch.members.CreatePrimary(ctx, m.ID)
		must(t, err)
		for _, v := range []store.Volume{w.v, func() store.Volume { a := w.vol(t, "db-archive"); w.ensure(t, a); return a }()} {
			v.InstanceID = &m.ID
			must(t, w.st.Volumes.Update(ctx, v))
		}
		if withStandby {
			must(t, w.st.OperatedMembers.Create(ctx, store.OperatedMember{ID: "m-std", InstanceID: m.ID, Role: opmember.Replica,
				NodeID: w.ctl.ID, Volume: "std-vol", State: opmember.Running, Phase: opmember.Streaming, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
		}
		j := w.quarantine(t, w.row)
		if j.State != job.Done {
			t.Fatalf("standby=%v job = %s %q", withStandby, j.State, j.Error)
		}
		if got := sawCall(w.wFake, "Stop("+w.cid); got == withStandby {
			t.Errorf("standby=%v: stopped = %v: %v", withStandby, got, callNames(w.wFake))
		}
		rec, _, _ := w.orch.quarLoad(ctx, w.row.ID)
		if slices.Contains(rec.Tiles, w.tl.ID) == withStandby {
			t.Errorf("standby=%v: record = %+v", withStandby, rec)
		}
		if withStandby {
			continue
		}
		if p, _ := w.r0(t, m.ID); p.State != opmember.Running {
			t.Errorf("the primary's member state = %s, want it kept running", p.State)
		}
		before := len(w.wFake.Calls())
		w.orch.startMembers(ctx)
		for _, c := range w.wFake.Calls()[before:] {
			if strings.HasPrefix(c.String(), "Start(") {
				t.Errorf("the tick started the primary on a missing disk: %v", c)
			}
		}
		w.check(t)
		if !w.deployJobs(t, w.tl) {
			t.Error("Check disk did not redeploy the standalone primary")
		}
	}
}

func (w *forgetWorld) r0(t *testing.T, instanceID string) (InstanceMember, bool) {
	t.Helper()
	ms, err := w.orch.members.List(context.Background(), instanceID)
	must(t, err)
	p, ok := opmember.PrimaryOf(ms)
	return p, ok
}

// Volumes and envs are not moved onto an erroring disk.
func TestMovesOntoAnErroringDiskAreRefused(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	v := d.vol(t, "data")
	d.ensure(t, v)
	for range diskMissingReads {
		d.watch(t, ioRead(row))
	}
	if _, err := d.orch.MoveVolumeToDisk(ctx, adminP(), v.ID); !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "failing") {
		t.Errorf("MoveVolumeToDisk = %v", err)
	}
	if _, err := d.orch.MoveAllToDisk(ctx, adminP(), "w1"); !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "failing") {
		t.Errorf("MoveAllToDisk = %v", err)
	}
	if err := d.orch.erroringDiskGate(ctx, "w1"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("gate = %v", err)
	}
	if err := d.orch.erroringDiskGate(ctx, d.ctl.ID); err != nil {
		t.Errorf("a node with a healthy disk: %v", err)
	}
	d.watch(t, readOf(row, 50*gig, 100*gig))
	if err := d.orch.erroringDiskGate(ctx, "w1"); err != nil {
		t.Errorf("after a good read: %v", err)
	}
}

// A standby whose drop from the sync list fails while the primary runs stays
// up: stopping it would hang the primary's commits.
func TestQuarantineKeepsAStandbyWhoseSyncDropFailed(t *testing.T) {
	w := newOpWorld(t)
	w.r.reloadBroken = true
	w.r.syncLive = `ANY 1 ("` + mflow.SlotName(w.rep.ID) + `")` // the primary waits for it, and a reload does not take
	j := w.quarantine(t, w.row)
	if j.State != job.Failed {
		t.Fatalf("job = %s, want failed", j.State)
	}
	if w.r.saw("stop " + w.rep.Container) {
		t.Error("the standby was stopped with the sync list still naming it")
	}
	if rec, ok, _ := w.orch.quarLoad(context.Background(), w.row.ID); !ok || rec.Done {
		t.Errorf("record = %+v, want one that is not done (the watch queues the job again)", rec)
	}
}

// A standby is not added on a node whose disk has failed.
func TestAddStandbyRefusedOnAFailedDisk(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	row := d.addDisk(t)
	r := newFailRigOn(t, d.twoNodes, false)
	for range diskMissingReads {
		d.watch(t, ioRead(row))
	}
	if _, err := d.orch.AddStandby(ctx, adminP(), r.inst.TileID, "w1"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("err = %v, want refused", err)
	}
}
