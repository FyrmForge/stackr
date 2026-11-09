package service

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	wseg1 = "000000010000000000000001"
	wseg2 = "000000010000000000000002"
	wseg3 = "000000010000000000000003"
)

// restoreRig is branchRig with the instance archiving to an off-box
// destination: one base finished 48h ago (WAL seg1..seg2 ship under it), the
// archive current to now.
type restoreRig struct {
	*branchRig
	s3   *fakeS3
	data store.Volume
	root string
	from time.Time // when the base finished
}

func newRestoreRig(t *testing.T) *restoreRig {
	t.Helper()
	r := &restoreRig{branchRig: newBranchRig(t)}
	ctx := context.Background()
	o := r.orch
	s3, srv := newFakeS3(t)
	r.s3 = s3
	r.sharedS3(t, srv.URL)
	r.inst.PITRDays = 7
	must(t, r.st.OperatedInstances.Update(ctx, r.inst))
	_, err := o.engines.Container(r.node(t, ctx), r.pg)
	must(t, err)
	if r.data, err = o.dataVolume(ctx, r.pg); err != nil {
		t.Fatal(err)
	}
	dest, err := o.pitrDest(ctx, r.org, r.data)
	must(t, err)
	r.root = backup.PITRRoot(r.org, r.data.ID)
	r.from = time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	run, err := o.backups.StartIn(ctx, backup.KindVolume, &r.data.ID, nil, dest.ID, "manual", nil)
	must(t, err)
	key := r.root + "/base/" + r.from.Format("20060102-150405") + "-" + wseg1 + "-pg-data.tar.gz.age"
	run, err = o.backups.SetWalStart(ctx, run, wseg1)
	must(t, err)
	stop := wseg2
	run.WalStop = &stop
	run, err = o.backups.Finish(ctx, run, key, 100, nil)
	must(t, err)
	run.FinishedAt = &r.from
	must(t, r.st.BackupRuns.Update(ctx, run))
	r.s3.objs[key] = []byte("base")
	r.s3.objs[backup.WALKey(r.root, wseg1, wseg2)] = []byte("wal")
	o.savePITRState(ctx, r.inst.ID, pitrState{ArchSince: r.from.Add(-time.Hour), LastShippedAt: time.Now(), LastCleanAt: time.Now()})
	return r
}

// A time inside the restorable range plans; outside it, or malformed, is
// refused with both bounds in the words; an instance with PITR off is
// refused before any archive is read.
func TestRestoreTimeIsCheckedAgainstTheArchive(t *testing.T) {
	t.Parallel()
	r := newRestoreRig(t)
	ctx := context.Background()
	o := r.orch
	src, err := o.pitrSourceOf(ctx, r.pg.ID)
	must(t, err)

	p, at, latest, err := o.pitrPlan(ctx, src, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), false)
	must(t, err)
	if latest || at.IsZero() || !strings.Contains(p.Base.ObjectKey, "/base/") || len(p.Bundles) != 1 {
		t.Errorf("plan = %+v at %v latest %v", p, at, latest)
	}
	if _, _, latest, err := o.pitrPlan(ctx, src, "latest", false); err != nil || !latest {
		t.Errorf("latest: %v %v", latest, err)
	}
	for name, in := range map[string]string{
		"before the base":   r.from.Add(-time.Hour).Format(time.RFC3339),
		"after the archive": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"not a time":        "yesterday",
	} {
		_, _, _, err := o.pitrPlan(ctx, src, in, false)
		if _, ok := errs.IsInvalid(err); !ok {
			t.Errorf("%s: err = %v, want invalid", name, err)
		}
		if name != "not a time" && (!strings.Contains(err.Error(), r.from.Format(time.RFC3339)) || !strings.Contains(err.Error(), "from ")) {
			t.Errorf("%s: %v does not name the range", name, err)
		}
	}
	// a branch within two minutes of now is told to branch without --at
	if _, _, _, err := o.pitrPlan(ctx, src, time.Now().Add(-30*time.Second).UTC().Format(time.RFC3339), true); err == nil {
		t.Error("a branch at now-30s was planned")
	}

	// a base whose WAL end has not shipped is not restorable
	must(t, func() error { delete(r.s3.objs, backup.WALKey(r.root, wseg1, wseg2)); return nil }())
	r.s3.objs[backup.WALKey(r.root, wseg1, wseg1)] = []byte("wal")
	if _, _, _, err := o.pitrPlan(ctx, src, "latest", false); err == nil || !strings.Contains(err.Error(), "no base backup") {
		t.Errorf("uncovered base: %v", err)
	}

	// PITR off: refused up front
	r.inst.PITRDays = 0
	must(t, r.st.OperatedInstances.Update(ctx, r.inst))
	if _, err := o.pitrSourceOf(ctx, r.pg.ID); err == nil || !strings.Contains(err.Error(), "off") {
		t.Errorf("pitr off: %v", err)
	}
}

// RestoreInstance and branch --at refuse what they must before anything is
// made: no signed-in user, a reader, a time out of range. No tile is left.
func TestRestoreInstanceAndBranchAtRefusals(t *testing.T) {
	t.Parallel()
	r := newRestoreRig(t)
	ctx := context.Background()
	o := r.orch
	tiles := func() int {
		ts, err := o.tiles.List(ctx, r.env)
		must(t, err)
		return len(ts)
	}
	n := tiles()
	good := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if _, _, err := o.RestoreInstance(ctx, nil, r.pg.ID, good); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("no user = %v", err)
	}
	if _, _, err := o.RestoreInstance(ctx, roleP(r.org, "viewer"), r.pg.ID, good); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("viewer = %v", err)
	}
	writer := roleP(r.org, "member")
	if _, _, err := o.RestoreInstance(ctx, writer, r.pg.ID, r.from.Add(-time.Hour).Format(time.RFC3339)); err == nil {
		t.Error("a time before the base restored")
	}
	// past the archive's reach (walLag) and inside the branch margin: refused
	if _, _, err := o.BranchSlice(ctx, writer, r.db.ID, BranchIn{At: time.Now().Add(-20 * time.Second).UTC().Format(time.RFC3339)}); err == nil {
		t.Error("a branch 20s ago went ahead")
	}
	if _, _, err := o.BranchSlice(ctx, writer, r.db.ID, BranchIn{At: r.from.Add(-time.Hour).Format(time.RFC3339)}); err == nil {
		t.Error("a branch before the base")
	}
	if got := tiles(); got != n {
		t.Errorf("%d tiles, was %d: a refused restore left rows", got, n)
	}
}

// A promote locks the source, its slice tiles, their consumers, the data
// volume and the copy; it refuses a tile that is no restored copy and a
// reader. The copy's volume gets no backups of its own.
func TestPromoteRestoreLocksAndRefusals(t *testing.T) {
	t.Parallel()
	r := newRestoreRig(t)
	ctx := context.Background()
	o := r.orch
	writer := roleP(r.org, "member")
	if _, err := o.PromoteRestore(ctx, writer, r.api.ID); err == nil || !strings.Contains(err.Error(), "not a restored copy") {
		t.Errorf("a plain tile promoted: %v", err)
	}
	src, err := o.pitrSourceOf(ctx, r.pg.ID)
	must(t, err)
	src.inst.Settings = map[string]string{"work_mem": "8MB"}
	cand, err := o.newCandidate(ctx, src, time.Now().Add(-time.Hour), "2026-11-07T10:00:00Z", false)
	must(t, err)
	if !strings.HasPrefix(cand.Slug, "pg-restored-") || cand.Kind != tile.Operated || cand.EnvironmentID != r.env {
		t.Fatalf("candidate = %q %s", cand.Slug, cand.Kind)
	}
	cm, err := o.operated.GetByTile(ctx, cand.ID)
	must(t, err)
	if cm.PITRDays != 0 || cm.AdminPassword != src.inst.AdminPassword {
		t.Errorf("candidate instance pitr_days %d, password carried = %v", cm.PITRDays, cm.AdminPassword == src.inst.AdminPassword)
	}
	// the restored stackr.conf is the source's: settings and major come along
	if cm.Settings["work_mem"] != "8MB" || cm.Major == 0 || cm.Major != src.inst.Major {
		t.Errorf("candidate settings %v major %d, source major %d", cm.Settings, cm.Major, src.inst.Major)
	}
	cv, _, err := o.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: r.env}, cand.Slug+"-data", &cm.ID)
	must(t, err)
	if ss, _ := o.backups.Schedules(ctx, cv.ID); len(ss) != 0 {
		t.Errorf("the copy's volume got schedules: %v", methodsOf(ss))
	}
	if _, err := o.PromoteRestore(ctx, roleP(r.org, "viewer"), cand.ID); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("viewer = %v", err)
	}
	j, err := o.PromoteRestore(ctx, writer, cand.ID)
	must(t, err)
	for _, want := range []string{r.pg.ID, r.db.ID, r.api.ID, cand.ID, "volume:" + r.data.ID} {
		if !slices.Contains(j.LockSet, want) {
			t.Errorf("lock set %v lacks %s", j.LockSet, want)
		}
	}
	// the org's job routes accept the job only when its payload names the org
	if c, err := o.ChildOf(ctx, "job", j.ID); err != nil || c.Org != r.org {
		t.Errorf("promote job placed in org %q (%v), want %q", c.Org, err, r.org)
	}
	// the copy never says it is done replaying here: the job refuses before touching anything
	if got := r.wait(t, j.ID); got.State != "failed" || !strings.Contains(got.Error, "not done replaying") {
		t.Errorf("job = %s: %s", got.State, got.Error)
	}
	if sv, _ := o.volumes.Get(ctx, r.data.ID); sv.Name != r.data.Name {
		t.Errorf("the source's volume row moved: %s", sv.Name)
	}
}

// The promote's steps run in the order the design gives, a failed step stops
// the rest and names itself, and nothing after the swap runs before it.
func TestPromoteStepsRunInOrder(t *testing.T) {
	t.Parallel()
	var got []string
	step := func(name string, err error) func() error {
		return func() error { got = append(got, name); return err }
	}
	steps := func(failAt string, swapped bool) promoteSteps {
		f := func(n string) func() error {
			if n == failAt {
				return step(n, errors.New("boom"))
			}
			return step(n, nil)
		}
		return promoteSteps{
			swapped: swapped,
			ready:   f("ready"), check: f("check"), history: f("history"), stopSrc: f("stopSrc"), swap: f("swap"),
			undo:     func() { got = append(got, "undo") },
			stopCand: f("stopCand"), start: f("start"), replay: f("replay"), drop: f("drop"), settle: f("settle"),
		}
	}
	all := []string{"ready", "check", "stopSrc", "swap", "stopCand", "start", "history", "replay", "settle", "drop"}
	var log strings.Builder
	if err := steps("", false).run(&log); err != nil || !slices.Equal(got, all) {
		t.Fatalf("order = %v, %v; want %v", got, err, all)
	}
	// A failure before the swap restarts the source and runs nothing after.
	got = nil
	err := steps("swap", false).run(&log)
	if err == nil || !strings.Contains(err.Error(), "swap the data volume") || !slices.Equal(got, []string{"ready", "check", "stopSrc", "swap", "undo"}) {
		t.Errorf("failing swap: ran %v, err %v", got, err)
	}
	// A failure after it is not undone: a re-run resumes instead.
	got = nil
	if err := steps("start", false).run(&log); err == nil || slices.Contains(got, "undo") || slices.Contains(got, "replay") {
		t.Errorf("failing start: ran %v, err %v", got, err)
	}
	// A failing history upload comes after the source started: it is not left
	// down, the candidate stays for the resume, and the archive reset waits.
	got = nil
	if err := steps("history", false).run(&log); err == nil || slices.Contains(got, "undo") || !slices.Contains(got, "start") || slices.Contains(got, "drop") {
		t.Errorf("failing history: ran %v, err %v", got, err)
	}
	// A reset that fails keeps the candidate, so the promote can be re-run.
	got = nil
	if err := steps("settle", false).run(&log); err == nil || slices.Contains(got, "drop") {
		t.Errorf("failing settle: ran %v, err %v", got, err)
	}
	// The conflict preflight stops everything before the source is touched.
	got = nil
	if err := steps("check", false).run(&log); err == nil || !slices.Equal(got, []string{"ready", "check", "undo"}) {
		t.Errorf("failing check: ran %v, err %v", got, err)
	}
	// The resume skips everything up to and including the swap.
	got = nil
	if err := steps("", true).run(&log); err != nil || !slices.Equal(got, all[4:]) {
		t.Errorf("resume ran %v, %v; want %v", got, err, all[4:])
	}
}

// swapData is all or nothing: when the copy's row cannot be written, the
// source's row keeps its volume.
func TestSwapDataIsAllOrNothing(t *testing.T) {
	t.Parallel()
	r := newRestoreRig(t)
	ctx := context.Background()
	ghost := store.Volume{ID: "no-such-row", Name: "ghost-vol"}
	if err := r.orch.swapData(ctx, r.data, ghost); err == nil {
		t.Fatal("swapData wrote a row that does not exist")
	}
	sv, err := r.orch.volumes.Get(ctx, r.data.ID)
	must(t, err)
	if sv.Name != r.data.Name {
		t.Errorf("source row = %s after a failed swap; want %s", sv.Name, r.data.Name)
	}
}

// The source's row takes the copy's Docker volume, the copy's row (orphaned)
// the old one: the old data stays, restorable until retention.
func TestSwapDataKeepsTheOldVolumeAsAnOrphan(t *testing.T) {
	t.Parallel()
	r := newRestoreRig(t)
	ctx := context.Background()
	o := r.orch
	src, err := o.pitrSourceOf(ctx, r.pg.ID)
	must(t, err)
	cand, err := o.newCandidate(ctx, src, time.Now(), "latest", false)
	must(t, err)
	cm, _ := o.operated.GetByTile(ctx, cand.ID)
	cv, _, err := o.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: r.env}, cand.Slug+"-data", &cm.ID)
	must(t, err)
	must(t, o.swapData(ctx, r.data, cv))
	sv, _ := o.volumes.Get(ctx, r.data.ID)
	cv2, _ := o.volumes.Get(ctx, cv.ID)
	if sv.Name != cv.Name || cv2.Name != r.data.Name || sv.OrphanedAt != nil || cv2.OrphanedAt == nil {
		t.Errorf("source row %s (orphaned %v), copy row %s (orphaned %v); want the names traded and only the copy's orphaned",
			sv.Name, sv.OrphanedAt != nil, cv2.Name, cv2.OrphanedAt != nil)
	}
}

func TestRoomErrorGivesTheNumbers(t *testing.T) {
	t.Parallel()
	if err := roomError("pg", 100<<30, 10<<30, 100); err != nil {
		t.Errorf("plenty of room: %v", err)
	}
	err := roomError("pg", 5<<30, 10<<30, 100)
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "needs") || !strings.Contains(err.Error(), "5.0 GB") {
		t.Errorf("err = %v", err)
	}
}

// candidate rows of tiles that are gone are swept; a live one stays.
func TestSweepCandidatesDropsRowsOfGoneTiles(t *testing.T) {
	t.Parallel()
	r := newRestoreRig(t)
	ctx := context.Background()
	o := r.orch
	must(t, o.setCandidate(ctx, "gone", candidate{Source: r.pg.ID, At: "latest", Created: time.Now()}))
	must(t, o.setCandidate(ctx, r.api.ID, candidate{Source: r.pg.ID, At: "latest", Created: time.Now()}))
	o.sweepCandidates(ctx)
	if _, ok := o.candidateOf(ctx, "gone"); ok {
		t.Error("the row of a gone tile stayed")
	}
	if _, ok := o.candidateOf(ctx, r.api.ID); !ok {
		t.Error("a live candidate's row was swept")
	}
}

// The restorable range ends walLag before the last clean tick: a write there
// may still sit in an open segment.
func TestRestorableToTrailsTheArchive(t *testing.T) {
	t.Parallel()
	if got := (pitrState{}).restorableTo(); !got.IsZero() {
		t.Errorf("nothing shipped = %v, want zero", got)
	}
	tick := time.Date(2026, 11, 7, 16, 48, 32, 0, time.UTC)
	st := pitrState{LastCleanAt: tick, LastShippedAt: tick.Add(-time.Minute)}
	if got := st.restorableTo(); !got.Equal(tick.Add(-walLag)) {
		t.Errorf("restorableTo = %v, want %v", got, tick.Add(-walLag))
	}
}

// Re-running a promote whose swap already committed resumes after it: the
// rows are not traded back, and the stopped copy does not block the re-run.
func TestPromoteRerunAfterTheSwapDoesNotSwapBack(t *testing.T) {
	t.Parallel()
	r := newRestoreRig(t)
	ctx := context.Background()
	o := r.orch
	src, err := o.pitrSourceOf(ctx, r.pg.ID)
	must(t, err)
	cand, err := o.newCandidate(ctx, src, time.Now(), "latest", false)
	must(t, err)
	cm, _ := o.operated.GetByTile(ctx, cand.ID)
	cv, _, err := o.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: r.env}, cand.Slug+"-data", &cm.ID)
	must(t, err)
	must(t, o.swapData(ctx, r.data, cv))
	// The copy has no running container: ready() would refuse it. The resume must not ask.
	_ = o.runPromoteRestore(ctx, &jobs.Run{Log: io.Discard}, promoteRestoreJob{SourceID: r.pg.ID, CandidateID: cand.ID})
	sv, _ := o.volumes.Get(ctx, r.data.ID)
	cv2, _ := o.volumes.Get(ctx, cv.ID)
	if sv.Name != cv.Name || cv2.Name != r.data.Name {
		t.Errorf("rows after the re-run: source %s, copy %s; want %s and %s (swapped back?)", sv.Name, cv2.Name, cv.Name, r.data.Name)
	}
}

// Every candidate slug leaves room for the "-data" volume under the volume
// name cap, long sources included, and two long ones sharing a prefix differ.
func TestRestoredSlugFitsTheVolumeNameCap(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 7, 10, 42, 0, 0, time.UTC)
	for _, src := range []string{"pg", "postgres", "orders-postgres", strings.Repeat("a-", 28) + "z"} {
		for _, temp := range []bool{false, true} {
			if err := volume.CheckName(restoredSlug(src, at, temp) + "-data"); err != nil {
				t.Errorf("%q temp=%v: %v", src, temp, err)
			}
		}
	}
	if got := restoredSlug("pg", at, false); got != "pg-restored-20261107-1042" {
		t.Errorf("short name changed: %s", got)
	}
	long := strings.Repeat("a", 45)
	if restoredSlug(long+"b", at, false) == restoredSlug(long+"c", at, false) {
		t.Error("two long names sharing a prefix collide")
	}
}

// The retry fires on postgres' "ran out of WAL before the target" and nothing
// else, and the config it refills with drops only the target.
func TestNoCommitAfterTargetRetryDecisionAndConf(t *testing.T) {
	r := newRestoreRig(t)
	ctx := context.Background()
	if !noCommitAfterTarget(errors.New("the replay stopped:\nFATAL:  recovery ended before configured recovery target was reached")) {
		t.Error("the end-of-WAL failure is not retried")
	}
	for _, err := range []error{nil, errors.New("requested timeline 2 is not a child of this server's history"), errors.New("could not locate a valid checkpoint record")} {
		if noCommitAfterTarget(err) {
			t.Errorf("%v is retried", err)
		}
	}
	at := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	first, _, _, err := r.orch.engines.RecoveryConf(ctx, r.pg, at)
	must(t, err)
	retry, _, _, err := r.orch.engines.RecoveryConf(ctx, r.pg, time.Time{})
	must(t, err)
	if !strings.Contains(first, "recovery_target_time =") {
		t.Fatalf("first conf has no target:\n%s", first)
	}
	if strings.Contains(retry, "recovery_target_time =") || strings.Contains(retry, "recovery_target_action") {
		t.Errorf("retry conf keeps the target:\n%s", retry)
	}
	for _, keep := range []string{"restore_command = ", "recovery_target_timeline = 'latest'"} {
		if !strings.Contains(retry, keep) {
			t.Errorf("retry conf lost %q:\n%s", keep, retry)
		}
	}
}

// A re-run of a swapped promote refuses once the source runs another copy's
// data: the job must not report the first copy promoted.
func TestPromoteResumeRefusesAfterAnotherCopyWasPromoted(t *testing.T) {
	t.Parallel()
	r := newRestoreRig(t)
	ctx := context.Background()
	o := r.orch
	src, err := o.pitrSourceOf(ctx, r.pg.ID)
	must(t, err)
	cand, err := o.newCandidate(ctx, src, time.Now().Add(-time.Hour), "2026-11-07T10:00:00Z", false)
	must(t, err)
	cm, err := o.operated.GetByTile(ctx, cand.ID)
	must(t, err)
	cv, _, err := o.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: r.env}, cand.Slug+"-data", &cm.ID)
	must(t, err)
	now := time.Now()
	cv.OrphanedAt = &now // the swap happened
	must(t, o.store.Volumes.Update(ctx, cv))
	c, _ := o.candidateOf(ctx, cand.ID)
	c.Vol = "stackr-vol-of-copy-a" // the source no longer names it
	must(t, o.setCandidate(ctx, cand.ID, c))
	j, err := o.PromoteRestore(ctx, roleP(r.org, "member"), cand.ID)
	must(t, err)
	if got := r.wait(t, j.ID); got.State != "failed" || !strings.Contains(got.Error, "another copy was promoted") {
		t.Errorf("job = %s: %s", got.State, got.Error)
	}
}

// A failover that lost WAL splits the range: the old base reaches up to the
// failover's mark, the newer base from its finish, PITRStatus shows both, a
// restore into the gap is refused and one on either side plans. A failover
// that lost nothing keeps one range. A restore never reads another archive
// epoch.
func TestFailoverGapSplitsTheRestorableRange(t *testing.T) {
	t.Parallel()
	r := newRestoreRig(t)
	ctx := context.Background()
	o := r.orch
	dest, err := o.pitrDest(ctx, r.org, r.data)
	must(t, err)
	// seg3 never shipped; a second base starts after it
	wseg4 := "000000010000000000000004"
	newFrom := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	run, err := o.backups.StartIn(ctx, backup.KindVolume, &r.data.ID, nil, dest.ID, "manual", nil)
	must(t, err)
	key := r.root + "/base/" + newFrom.Format("20060102-150405") + "-" + wseg4 + "-pg-data.tar.gz.age"
	run, err = o.backups.SetWalStart(ctx, run, wseg4)
	must(t, err)
	run.WalStop = &wseg4
	run, err = o.backups.Finish(ctx, run, key, 100, nil)
	must(t, err)
	run.FinishedAt = &newFrom
	must(t, r.st.BackupRuns.Update(ctx, run))
	r.s3.objs[key] = []byte("base")
	r.s3.objs[backup.WALKey(r.root, wseg4, wseg4)] = []byte("wal")

	mark := time.Now().Add(-30 * time.Hour).UTC().Truncate(time.Second)
	st, _ := o.pitrState(ctx, r.inst.ID)
	st.Marks = []backup.Mark{{At: mark, Seg: wseg2}}
	o.savePITRState(ctx, r.inst.ID, st)

	h, err := o.PITRStatus(ctx, r.pg.ID)
	must(t, err)
	if len(h.Ranges) != 2 || !h.Ranges[0].From.Equal(r.from) || !h.Ranges[0].To.Equal(mark) || !h.Ranges[1].From.Equal(newFrom) {
		t.Fatalf("ranges = %+v, want the old base to the mark and the new base on", h.Ranges)
	}
	src, err := o.pitrSourceOf(ctx, r.pg.ID)
	must(t, err)
	plan := func(ago time.Duration) error {
		_, _, _, err := o.pitrPlan(ctx, src, time.Now().Add(-ago).UTC().Format(time.RFC3339), false)
		return err
	}
	if err := plan(40 * time.Hour); err != nil {
		t.Errorf("a time before the gap: %v", err)
	}
	if err := plan(10 * time.Hour); err == nil || !strings.Contains(err.Error(), "gap") {
		t.Errorf("a time in the gap: %v", err)
	}
	if err := plan(30 * time.Minute); err != nil {
		t.Errorf("a time after the gap: %v", err)
	}

	// nothing lost: seg3 is there, one range
	r.s3.objs[backup.WALKey(r.root, wseg3, wseg3)] = []byte("wal")
	if h, err = o.PITRStatus(ctx, r.pg.ID); err != nil || len(h.Ranges) != 0 || h.From == nil || !h.From.Equal(r.from) {
		t.Errorf("clean failover: %+v %v, want one range from the first base", h, err)
	}
	if err := plan(10 * time.Hour); err != nil {
		t.Errorf("a clean failover refused a time: %v", err)
	}

	// another archive epoch is another archive: none of the above is visible
	r.inst.ArchiveEpoch = 1
	must(t, r.st.OperatedInstances.Update(ctx, r.inst))
	if src, err = o.pitrSourceOf(ctx, r.pg.ID); err != nil {
		t.Fatal(err)
	}
	if err := plan(30 * time.Minute); err == nil {
		t.Error("a restore read across archive epochs")
	}
}

// With one bounded range (the newer base is not restorable yet) the status
// ends at the failover's mark, not at the shipper's last tick.
func TestPITRStatusEndsAtTheGapWhenOnlyTheOldRangeIsLeft(t *testing.T) {
	t.Parallel()
	r := newRestoreRig(t)
	ctx := context.Background()
	o := r.orch
	r.s3.objs[backup.WALKey(r.root, "000000010000000000000004", "000000010000000000000004")] = []byte("wal")
	mark := time.Now().Add(-30 * time.Hour).UTC().Truncate(time.Second)
	st, _ := o.pitrState(ctx, r.inst.ID)
	st.Marks = []backup.Mark{{At: mark, Seg: wseg2}}
	o.savePITRState(ctx, r.inst.ID, st)
	h, err := o.PITRStatus(ctx, r.pg.ID)
	must(t, err)
	if len(h.Ranges) != 0 || h.From == nil || !h.From.Equal(r.from) || h.To == nil || !h.To.Equal(mark) {
		t.Errorf("status = from %v to %v ranges %v, want %v to %v", h.From, h.To, h.Ranges, r.from, mark)
	}
	// a mark on another timeline explains nothing: the old base reaches nowhere
	st.Marks = []backup.Mark{{At: mark, Seg: "000000020000000000000002"}}
	o.savePITRState(ctx, r.inst.ID, st)
	if h, err = o.PITRStatus(ctx, r.pg.ID); err != nil || h.From != nil {
		t.Errorf("unexplained hole advertised: %+v %v", h.From, err)
	}
}
