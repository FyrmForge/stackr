package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
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
	must(t, r.st.ManagedInstances.Update(ctx, r.inst))
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
	o.savePITRState(ctx, r.inst.ID, pitrState{LastShippedAt: time.Now(), LastCleanAt: time.Now()})
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
	must(t, r.st.ManagedInstances.Update(ctx, r.inst))
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
	if _, _, err := o.BranchSlice(ctx, writer, r.db.ID, BranchIn{At: time.Now().Add(-20 * time.Second).UTC().Format(time.RFC3339)}); err == nil ||
		!strings.Contains(err.Error(), "branch without --at") {
		t.Errorf("branch at now-20s = %v", err)
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
	cand, err := o.newCandidate(ctx, src, time.Now().Add(-time.Hour), "2026-11-07T10:00:00Z", false)
	must(t, err)
	if !strings.HasPrefix(cand.Slug, "pg-restored-") || cand.Kind != tile.Managed || cand.EnvironmentID != r.env {
		t.Fatalf("candidate = %q %s", cand.Slug, cand.Kind)
	}
	cm, err := o.managed.GetByTile(ctx, cand.ID)
	must(t, err)
	if cm.PITRDays != 0 || cm.AdminPassword != src.inst.AdminPassword {
		t.Errorf("candidate instance pitr_days %d, password carried = %v", cm.PITRDays, cm.AdminPassword == src.inst.AdminPassword)
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
	steps := func(failAt string) promoteSteps {
		f := func(n string) func() error {
			if n == failAt {
				return step(n, errors.New("boom"))
			}
			return step(n, nil)
		}
		return promoteSteps{
			ready: f("ready"), history: f("history"), stopCand: f("stopCand"), stopSrc: f("stopSrc"), swap: f("swap"),
			start: f("start"), replay: f("replay"), drop: f("drop"), settle: f("settle"),
		}
	}
	all := []string{"ready", "history", "stopCand", "stopSrc", "swap", "start", "replay", "drop", "settle"}
	var log strings.Builder
	if err := steps("").run(&log); err != nil || !slices.Equal(got, all) {
		t.Fatalf("order = %v, %v; want %v", got, err, all)
	}
	got = nil
	err := steps("swap").run(&log)
	if err == nil || !strings.Contains(err.Error(), "swap the data volume") || !slices.Equal(got, all[:5]) {
		t.Errorf("failing swap: ran %v, err %v", got, err)
	}
	for _, s := range []string{"start", "replay", "drop", "settle"} {
		if slices.Contains(got, s) {
			t.Errorf("%s ran after the swap failed", s)
		}
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
	cm, _ := o.managed.GetByTile(ctx, cand.ID)
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
