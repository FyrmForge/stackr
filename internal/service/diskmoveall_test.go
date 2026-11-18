package service

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Move all to disk (work package 2N). The rig is 2M's: the plain volume "data"
// mounted by tile "db" (100 bytes) on w1 with a ready disk.

// logs is a second plain volume (50 bytes) with its own tile "web", so it is a
// group of its own.
func (r *moveRig) logs(t *testing.T) (store.Volume, Tile) {
	t.Helper()
	v := r.volume(t, "logs")
	return v, r.withContainer(t, "web", "logs:/logs")
}

// withContainer is a tile with a running replica "c-<name>".
func (r *moveRig) withContainer(t *testing.T, name, volumes string) Tile {
	t.Helper()
	tl := r.diskTile(t, name, volumes)
	r.wFake.Containers = append(r.wFake.Containers, docker.Container{ID: "c-" + name, Name: name, State: "running",
		Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"}})
	return tl
}

// stopsBeforeDeploy counts the removals of a container before the first
// deploy (a deploy replaces the old container itself).
func (r *moveRig) stopsBeforeDeploy(id string) int {
	n := 0
	for _, c := range callNames(r.wFake) {
		if strings.HasPrefix(c, "Create(") {
			break
		}
		if strings.HasPrefix(c, "StopRemove("+id) {
			n++
		}
	}
	return n
}

func (r *moveRig) moveAll(t *testing.T) []Job {
	t.Helper()
	js, err := r.orch.MoveAllToDisk(context.Background(), adminP(), "w1")
	must(t, err)
	return js
}

func (r *moveRig) waitAll(t *testing.T, js []Job) []Job {
	t.Helper()
	for i := range js {
		js[i] = r.wait(t, js[i])
	}
	return js
}

func (r *moveRig) plan(t *testing.T) MoveAllPlan {
	t.Helper()
	p, err := r.orch.PlanMoveAllToDisk(context.Background(), adminP(), "w1")
	must(t, err)
	return p
}

func groupSlugs(g MoveAllGroup) string {
	var s []string
	for _, v := range g.Volumes {
		s = append(s, v.Slug)
	}
	slices.Sort(s)
	return strings.Join(s, ",")
}

func TestMoveAllListsAndSkipsInstancesWithStandbys(t *testing.T) {
	ctx := context.Background()
	r := newMoveRig(t)
	logs, _ := r.logs(t)
	m, err := r.orch.operated.Create(ctx, r.tl.ID, "postgres", "stackr", "")
	must(t, err)
	v := r.v
	v.InstanceID = &m.ID
	must(t, r.st.Volumes.Update(ctx, v))
	must(t, r.st.OperatedMembers.Create(ctx, store.OperatedMember{ID: "m-std", InstanceID: m.ID, Role: opmember.Replica,
		NodeID: "w1", Volume: "std-vol", State: opmember.Running, Phase: opmember.Streaming, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	p := r.plan(t)
	if len(p.Groups) != 1 || p.Groups[0].Volumes[0].ID != logs.ID {
		t.Errorf("groups = %+v", p.Groups)
	}
	if len(p.Skipped) != 1 || p.Skipped[0].ID != r.v.ID || !strings.Contains(p.Skipped[0].Reason, "standbys") {
		t.Errorf("skipped = %+v", p.Skipped)
	}
}

func TestMoveAllRefusedWithoutRoom(t *testing.T) {
	r := newMoveRig(t)
	r.logs(t)
	r.facts(t, diskFacts(120)) // each fits alone, 150 bytes and a tenth do not
	if p := r.plan(t); p.Fits || p.Need != 165 || p.Free != 120 {
		t.Errorf("plan need %d free %d fits %v", p.Need, p.Free, p.Fits)
	}
	js, err := r.orch.MoveAllToDisk(context.Background(), adminP(), "w1")
	if _, ok := errors.AsType[errs.Conflict](err); !ok || len(js) != 0 {
		t.Fatalf("err = %v, jobs %d, want refused for room", err, len(js))
	}
	r.facts(t, diskFacts(1<<30))
	r.waitAll(t, r.moveAll(t))
}

// Concurrent presses: one enqueues every group, the rest are refused, so no
// two calls split the groups between them.
func TestMoveAllConcurrentPressesOneEnqueuesTheRestAreRefused(t *testing.T) {
	r := newMoveRig(t)
	r.logs(t)
	release := r.blockCopy()
	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	jobsOf := make([][]Job, n)
	errsOf := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			jobsOf[i], errsOf[i] = r.orch.MoveAllToDisk(context.Background(), adminP(), "w1")
		}()
	}
	close(start)
	wg.Wait()
	ok, total := 0, 0
	for i := range n {
		total += len(jobsOf[i])
		if errsOf[i] == nil {
			ok++
		} else if _, c := errors.AsType[errs.Conflict](errsOf[i]); !c {
			t.Errorf("press %d: %v, want a conflict", i, errsOf[i])
		}
	}
	if ok != 1 || total != 2 {
		t.Errorf("%d presses enqueued %d jobs, want 1 press and the 2 groups", ok, total)
	}
	release()
	for _, js := range jobsOf {
		r.waitAll(t, js)
	}
}

func TestMoveAllEnqueuesOneJobPerGroupSmallestFirst(t *testing.T) {
	r := newMoveRig(t)
	logs, _ := r.logs(t)
	release := r.blockCopy()
	js := r.moveAll(t)
	if len(js) != 2 || !strings.Contains(js[0].Payload, logs.ID) || !strings.Contains(js[1].Payload, r.v.ID) {
		t.Fatalf("jobs = %+v, want logs (50) then data (100)", js)
	}
	release()
	for _, j := range r.waitAll(t, js) {
		if j.State != job.Done {
			t.Errorf("job %s %q", j.State, j.Error)
		}
	}
	r.wantMoved(t, r.v)
	r.wantMoved(t, logs)
}

// A-V1-B-V2-C is one group and B stops once.
func TestMoveAllGroupIsAConnectedComponent(t *testing.T) {
	r := newMoveRig(t)
	logs := r.volume(t, "logs")
	r.withContainer(t, "b", "data:/data\nlogs:/logs")
	r.withContainer(t, "c", "logs:/logs")
	p := r.plan(t)
	if len(p.Groups) != 1 || groupSlugs(p.Groups[0]) != "data,logs" || len(p.Groups[0].Tiles) != 3 {
		t.Fatalf("groups = %+v", p.Groups)
	}
	js := r.waitAll(t, r.moveAll(t))
	if len(js) != 1 || js[0].State != job.Done {
		t.Fatalf("jobs = %+v", js)
	}
	r.wantMoved(t, r.v)
	r.wantMoved(t, logs)
	if n := r.stopsBeforeDeploy("c-b"); n != 1 {
		t.Errorf("B stopped %d times, want once", n)
	}
}

func TestMoveAllGroupLockSetHasTheTodiskKeyAndTheEnvConfigLock(t *testing.T) {
	r := newMoveRig(t)
	logs, tl := r.logs(t)
	js := r.moveAll(t)
	for _, j := range js {
		for _, want := range []string{"todisk:w1", "env:" + r.edge.ID} {
			if !slices.Contains(j.LockSet, want) {
				t.Errorf("lock set %v lacks %q", j.LockSet, want)
			}
		}
		if slices.Contains(j.LockSet, "node:w1") {
			t.Errorf("lock set %v holds the node", j.LockSet)
		}
	}
	if !slices.Contains(js[0].LockSet, "volume:"+logs.ID) || !slices.Contains(js[0].LockSet, tl.ID) || slices.Contains(js[0].LockSet, r.tl.ID) {
		t.Errorf("first group lock set %v", js[0].LockSet)
	}
	r.waitAll(t, js)
}

// DL: a second press never cancels or doubles a live group job.
func TestMoveAllSecondPressRefusedWhileATodiskJobIsLive(t *testing.T) {
	ctx := context.Background()
	r := newMoveRig(t)
	r.logs(t)
	release := r.blockCopy()
	js := r.moveAll(t)
	more, err := r.orch.MoveAllToDisk(ctx, adminP(), "w1")
	if _, ok := errors.AsType[errs.Conflict](err); !ok || len(more) != 0 || !strings.Contains(err.Error(), "already moving") {
		t.Fatalf("second press = %v (%d jobs), want refused", err, len(more))
	}
	if p := r.plan(t); !p.Busy {
		t.Error("the plan does not say busy")
	}
	for _, j := range js {
		if got, _ := r.orch.GetJob(ctx, j.ID); got.State == job.Cancelled || got.State == job.Failed {
			t.Errorf("job is %s after the second press", got.State)
		}
	}
	release()
	for _, j := range r.waitAll(t, js) {
		if j.State != job.Done {
			t.Errorf("job %s %q", j.State, j.Error)
		}
	}
}

func TestMoveAllRefusedWhileDiskNotReady(t *testing.T) {
	r := newMoveRig(t)
	setState(t, r.diskTest, r.row, disk.StateMissing)
	js, err := r.orch.MoveAllToDisk(context.Background(), adminP(), "w1")
	if !errors.Is(err, errs.ErrRefused) || len(js) != 0 {
		t.Fatalf("err = %v, jobs %d", err, len(js))
	}
}

// Each group's tiles are down only during its own copy.
func TestMoveAllOneTileDownAtATime(t *testing.T) {
	r := newMoveRig(t)
	r.logs(t)
	release := r.blockCopy()
	js := r.moveAll(t)
	for range 300 { // the first group (logs, web) is copying
		if sawCall(r.wFake, "EnsureTool") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sawCall(r.wFake, "StopRemove(c-db") {
		t.Errorf("db was stopped while the first group copies: %v", callNames(r.wFake))
	}
	release()
	for _, j := range r.waitAll(t, js) {
		if j.State != job.Done {
			t.Errorf("job %s %q", j.State, j.Error)
		}
	}
}

// DL: queued group jobs survive a restart; the one that was running is
// failed at start, and only its group stays on the old copy.
func TestMoveAllQueuedJobsSurviveARestartAndACrashLosesOnlyTheRunningGroup(t *testing.T) {
	ctx := context.Background()
	r := newMoveRig(t)
	logs, _ := r.logs(t)
	r.orch.jobs.Close() // the process dies: nothing runs until the new runner starts
	// a group job that was running when it died: it holds the todisk key
	r.holder(t, string(kindVolumeToDisk), todiskKey("w1"), "volume:"+r.v.ID, r.tl.ID)
	rows, err := r.orch.jobRows.List(ctx, job.Running)
	must(t, err)
	var dead store.Job
	for _, j := range rows {
		if j.Kind == string(kindVolumeToDisk) {
			dead = j
		}
	}
	queued, err := r.orch.enqueueVolumeMove(ctx, adminP(), "w1", []Volume{logs})
	must(t, err)
	if got, _ := r.orch.GetJob(ctx, queued.ID); got.State != job.Queued {
		t.Fatalf("the queued job is %s", got.State)
	}
	rn := jobs.New(r.orch.jobRows, r.orch.handlers(), t.TempDir(), jobs.Options{})
	must(t, rn.Start(ctx))
	t.Cleanup(rn.Close)
	if got, _ := r.orch.GetJob(ctx, dead.ID); got.State != job.Failed || !strings.Contains(got.Error, "restarted") {
		t.Errorf("the running job is %s %q", got.State, got.Error)
	}
	if q := r.wait(t, queued); q.State != job.Done {
		t.Fatalf("queued job: %s %q", q.State, q.Error)
	}
	r.wantMoved(t, logs)
	if got := r.get(t, r.v.ID); got.Name != r.v.Name {
		t.Errorf("the crashed group's volume names %q", got.Name)
	}
}

// DL: a group whose second copy fails: the first is swapped, the tiles deploy
// once on the mixed set, the old copy goes only through its orphan row, the
// volume never attempted is marked skipped, and no tile stops a second time.
func TestMoveAllGroupOfTwoFailureOnTheSecondDeploysTheMixedSetOnceCleansOldCopiesViaOrphanRowsAndMarksTheRestSkipped(t *testing.T) {
	r := newMoveRig(t)
	logs := r.volume(t, "logs")
	third := r.volume(t, "zeta")
	r.withContainer(t, "app", "data:/data\nlogs:/logs\nzeta:/z")
	r.wFake.Err = map[string]error{"UntarVolume(" + moveTarget(logs.ID) + ")": errors.New("disk write failed")}
	js := r.waitAll(t, r.moveAll(t))
	if len(js) != 1 || js[0].State != job.Failed || !strings.Contains(js[0].Error, "disk write failed") {
		t.Fatalf("jobs = %+v", js)
	}
	r.wantMoved(t, r.v)
	for _, v := range []store.Volume{logs, third} {
		if got := r.get(t, v.ID); got.Name != v.Name {
			t.Errorf("%s now names %q", v.Slug, got.Name)
		}
		if _, ok := r.orphanNamed(t, v.Name); ok {
			t.Errorf("%s has an orphan row", v.Slug)
		}
	}
	if sawCall(r.wFake, "RemoveVolume("+r.v.Name+")") {
		t.Errorf("an old copy was removed outside the orphan job: %v", callNames(r.wFake))
	}
	if sawCall(r.wFake, "UntarVolume("+moveTarget(third.ID)+")") {
		t.Errorf("the volume after the failure was attempted: %v", callNames(r.wFake))
	}
	for _, c := range []string{"c-app", "c-db"} {
		if n := r.stopsBeforeDeploy(c); n != 1 {
			t.Errorf("%s stopped %d times before the deploy, want once", c, n)
		}
	}
	if n := countCalls(r.wFake, "Create(stackr-app"); n != 1 {
		t.Errorf("app deployed %d times, want once", n)
	}
	if b, err := os.ReadFile(js[0].LogPath); err != nil || !strings.Contains(string(b), "skipped") {
		t.Errorf("the job log does not mark the unattempted volume skipped: %q %v", b, err)
	}
}

// DL: no tile of a group is stopped a second time.
func TestMoveAllGroupNeverStopsItsTilesASecondTime(t *testing.T) {
	r := newMoveRig(t)
	r.volume(t, "logs")
	r.withContainer(t, "app", "data:/data\nlogs:/logs")
	r.waitAll(t, r.moveAll(t))
	for _, c := range []string{"c-app", "c-db"} {
		if n := r.stopsBeforeDeploy(c); n != 1 {
			t.Errorf("%s stopped %d times before the deploy, want once", c, n)
		}
	}
}

// crash and resume

// DL: a job stopped in the middle of a copy leaves the row on the old copy;
// pressing again moves it, discarding the partial copy, never untarring over
// a live volume.
func TestMoveAllRerunFinishesAHalfMovedVolumeAfterACopyCrash(t *testing.T) {
	r := newMoveRig(t)
	r.crash(t, "copy", r.v)
	r.wantUntouched(t, r.v)
	if p := r.plan(t); len(p.Groups) != 1 || p.Groups[0].Volumes[0].Finishes {
		t.Fatalf("plan = %+v, want a fresh move", p.Groups)
	}
	r.waitAll(t, r.moveAll(t))
	r.wantMoved(t, r.v)
}

// DL: a job stopped after the row swap: the plan lists the volume as "will
// finish", the new job only deploys, and nothing is untarred again.
func TestMoveAllRerunFinishesAHalfMovedVolumeAfterTheSwap(t *testing.T) {
	r := newMoveRig(t)
	r.crash(t, "swap", r.v)
	r.wFake.Containers = nil // the crash left the tile's container removed
	untars := countCalls(r.wFake, "UntarVolume(")
	p := r.plan(t)
	if len(p.Groups) != 1 || !p.Groups[0].Volumes[0].Finishes || p.Groups[0].SizeBytes != 0 || len(p.Skipped) != 0 {
		t.Fatalf("plan = %+v, want will finish", p)
	}
	js := r.waitAll(t, r.moveAll(t))
	if js[0].State != job.Done {
		t.Fatalf("job %s %q", js[0].State, js[0].Error)
	}
	if n := countCalls(r.wFake, "UntarVolume("); n != untars {
		t.Errorf("untar ran %d times after the crash, want %d", n, untars)
	}
	if !sawCall(r.wFake, "Create(") {
		t.Errorf("the tile was not deployed: %v", callNames(r.wFake))
	}
	r.wantMoved(t, r.v)
}

// DL: a group stopped after its first copy has deployed nothing and swapped
// nothing; the rerun deploys only once every volume is copied.
func TestMoveAllGroupResumeNeverDeploysBeforeEveryVolumeOfTheGroupIsCopied(t *testing.T) {
	r := newMoveRig(t)
	logs := r.volume(t, "logs")
	r.withContainer(t, "app", "data:/data\nlogs:/logs")
	r.crash(t, "check", r.v, logs) // after the first volume's check
	if sawCall(r.wFake, "Create(") {
		t.Fatalf("a deploy ran before the whole group was copied: %v", callNames(r.wFake))
	}
	r.wantUntouched(t, r.v)
	r.wantUntouched(t, logs)
	js := r.waitAll(t, r.moveAll(t))
	if len(js) != 1 || js[0].State != job.Done {
		t.Fatalf("jobs = %+v", js)
	}
	r.wantMoved(t, r.v)
	r.wantMoved(t, logs)
	if n := countCalls(r.wFake, "Create(stackr-app"); n != 1 {
		t.Errorf("app deployed %d times", n)
	}
}
