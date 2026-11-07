package service

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func TestRestorableToIgnoresAPartlyShippedBacklog(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for name, c := range map[string]struct {
		st   pitrState
		want time.Time
	}{
		"nothing yet":                {pitrState{}, time.Time{}},
		"shipped but never clean":    {pitrState{LastShippedAt: now}, time.Time{}},
		"clean":                      {pitrState{LastCleanAt: now.Add(-time.Hour)}, now.Add(-time.Hour - walLag)},
		"a later partial ship loses": {pitrState{LastCleanAt: now.Add(-time.Hour), LastShippedAt: now}, now.Add(-time.Hour - walLag)},
	} {
		if got := c.st.restorableTo(); !got.Equal(c.want) {
			t.Errorf("%s: restorableTo = %v, want %v", name, got, c.want)
		}
	}
}

// pitrRig is an archiving instance whose container answers the shipper.
type pitrRig struct {
	*world
	pg   Tile
	m    store.ManagedInstance
	data store.Volume
	root string
	lsOK *bool // false: the container has no archive dir
}

func newPITRRig(t *testing.T) *pitrRig {
	t.Helper()
	w := newWorld(t)
	ctx := context.Background()
	_, srv := newFakeS3(t)
	w.sharedS3(t, srv.URL)
	pg, err := w.orch.CreateManagedTile(ctx, Tile{EnvironmentID: w.env, Name: "pg"}, "postgres")
	must(t, err)
	m, err := w.orch.managed.GetByTile(ctx, pg.ID)
	must(t, err)
	_, err = w.orch.engines.Container(w.node(t, ctx), pg)
	must(t, err)
	data, err := w.orch.dataVolume(ctx, pg)
	must(t, err)
	w.fake.Containers = []docker.Container{{
		ID: "c-pg", Name: "pg", State: "running",
		Labels: map[string]string{tile.LabelTile: pg.ID, tile.LabelRole: "replica"},
	}}
	w.fake.ExecFn = func(cmd []string) string {
		if slices.Contains(cmd, "pg_stat_archiver") {
			return "0\n"
		}
		return ""
	}
	return &pitrRig{world: w, pg: pg, m: m, data: data, root: backup.PITRRoot(w.org, data.ID), lsOK: new(bool)}
}

// noArchive makes the container's `ls` of the archive dir fail, as a
// container started without the archive volume does.
func (r *pitrRig) noArchive(on bool) {
	if on {
		r.fake.Err = map[string]error{"ExecStream(c-pg, ls, -1, " + mflow.PGArchiveDir + ")": errors.New("no such directory")}
	} else {
		r.fake.Err = nil
	}
}

func (r *pitrRig) backupJobs(t *testing.T) int {
	t.Helper()
	js, err := r.st.Jobs.ListByState(context.Background())
	must(t, err)
	n := 0
	for _, j := range js {
		if j.Kind == string(kindBackup) {
			n++
		}
	}
	return n
}

// The first base waits for a container that archives: on the old container,
// started before the redeploy, its WAL would never be kept.
func TestPITRFirstBaseWaitsForAnArchivingContainer(t *testing.T) {
	t.Parallel()
	r := newPITRRig(t)
	ctx := context.Background()
	r.noArchive(true)
	r.orch.pitrTick(ctx)
	if n := r.backupJobs(t); n != 0 {
		t.Fatalf("%d base backups queued on a container without the archive", n)
	}
	if st, _ := r.orch.pitrState(ctx, r.m.ID); !st.ArchSince.IsZero() {
		t.Errorf("archiving since %v with no archive dir", st.ArchSince)
	}
	r.noArchive(false)
	r.orch.pitrTick(ctx)
	if n := r.backupJobs(t); n != 1 {
		t.Fatalf("%d base backups queued once it archives, want 1", n)
	}
	if st, _ := r.orch.pitrState(ctx, r.m.ID); st.ArchSince.IsZero() {
		t.Error("archiving start not recorded")
	}
}

// Only a base on the active destination that started once the container
// archived counts as the first base.
func TestPITRFirstBaseCountsOnlyThisDestinationAndEpoch(t *testing.T) {
	t.Parallel()
	r := newPITRRig(t)
	ctx := context.Background()
	o := r.orch
	dest, err := o.pitrDest(ctx, r.org, r.data)
	must(t, err)
	run, err := o.backups.StartIn(ctx, backup.KindVolume, &r.data.ID, nil, dest.ID, "schedule", nil)
	must(t, err)
	_, err = o.backups.Finish(ctx, run, r.root+"/base/20261107-000000-data.tar.gz.age", 1, nil)
	must(t, err)
	now := time.Now()
	for name, c := range map[string]struct {
		dest  store.BackupDest
		since time.Time
		want  int
	}{
		"a base of this run":                            {dest, now.Add(-time.Hour), 0},
		"started before archiving began (PITR was off)": {dest, now.Add(time.Hour), 1},
		"another destination":                           {store.BackupDest{ID: "elsewhere"}, now.Add(-time.Hour), 1},
	} {
		before := r.backupJobs(t)
		o.pitrFirstBase(ctx, r.data, c.dest, &pitrState{ArchSince: c.since}, now)
		if got := r.backupJobs(t) - before; got != c.want {
			t.Errorf("%s: %d base backups queued, want %d", name, got, c.want)
		}
	}
}

// Bases from before archiving began, or on another destination, never plan a
// restore: a `latest` restore must not return the data from when PITR was off.
func TestRestoreIgnoresBasesBeforeTheEpochAndOnOtherDestinations(t *testing.T) {
	t.Parallel()
	r := newRestoreRig(t)
	ctx := context.Background()
	o := r.orch
	src, err := o.pitrSourceOf(ctx, r.pg.ID)
	must(t, err)
	if _, _, _, err := o.pitrPlan(ctx, src, "latest", false); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	st, _ := o.pitrState(ctx, r.inst.ID)
	st.ArchSince = time.Now().Add(time.Hour) // archiving (re)started after the base was taken
	o.savePITRState(ctx, r.inst.ID, st)
	if _, _, _, err := o.pitrPlan(ctx, src, "latest", false); !isConflict(err) {
		t.Errorf("a base from before the gap planned a restore: %v", err)
	}
	if h, err := o.PITRStatus(ctx, r.pg.ID); err != nil || h.From != nil {
		t.Errorf("status range from %v (%v), want none", h.From, err)
	}

	st.ArchSince = time.Now().Add(-72 * time.Hour)
	o.savePITRState(ctx, r.inst.ID, st)
	runs, err := o.backups.Runs(ctx, r.data.ID)
	must(t, err)
	other, err := o.backups.For(ctx, r.org, runs[0].DestID)
	must(t, err)
	other.ID, other.Name = uuid.NewString(), "elsewhere"
	must(t, r.st.BackupDests.Create(ctx, other))
	for _, run := range runs {
		run.DestID = other.ID
		must(t, r.st.BackupRuns.Update(ctx, run))
	}
	if _, _, _, err := o.pitrPlan(ctx, src, "latest", false); !isConflict(err) {
		t.Errorf("a base on another destination planned a restore: %v", err)
	}
}

// Turning PITR off keeps the shipper draining while the running container
// still archives; the bookkeeping goes once the restarted container has no
// archive dir.
func TestSetPITROffKeepsShippingUntilTheContainerStopsArchiving(t *testing.T) {
	t.Parallel()
	r := newPITRRig(t)
	ctx := context.Background()
	o := r.orch
	r.orch.pitrTick(ctx)
	_, _, err := o.SetPITR(ctx, r.pg.ID, 0)
	must(t, err)
	if ss, _ := o.BackupSchedules(ctx, r.data.ID); !slices.Contains(methodsOf(ss), "base") {
		t.Fatalf("schedules dropped before the container stopped archiving: %v", methodsOf(ss))
	}
	if _, ok := o.pitrState(ctx, r.m.ID); !ok {
		t.Fatal("shipper state dropped before the container stopped archiving")
	}
	before := len(r.fake.Calls())
	o.pitrTick(ctx) // the old container still archives
	if len(r.fake.Calls()) == before {
		t.Error("the archive was not looked at while the container still archives")
	}
	if _, ok := o.pitrState(ctx, r.m.ID); !ok {
		t.Fatal("state dropped while archiving")
	}
	if n := r.backupJobs(t); n != 1 { // only the first base of the on-state, none new while off
		t.Errorf("%d base backups, want the 1 from before", n)
	}

	r.noArchive(true) // the redeploy without the archive volume
	o.pitrTick(ctx)
	if _, ok := o.pitrState(ctx, r.m.ID); ok {
		t.Error("state outlived the container's archiving")
	}
	if ss, _ := o.BackupSchedules(ctx, r.data.ID); slices.Contains(methodsOf(ss), "base") {
		t.Errorf("base schedule outlived it: %v", methodsOf(ss))
	}
}

// A redeploy that cannot be queued leaves the window as it was.
func TestSetPITRRollsBackWhenTheRedeployIsRefused(t *testing.T) {
	t.Parallel()
	r := newPITRRig(t)
	ctx := context.Background()
	o := r.orch
	must(t, r.st.Nodes.Create(ctx, store.Node{
		ID: "w9", Name: "w9", Slug: "w9", Role: node.Worker, Tags: "[]", RangeCIDR: "10.219.0.0/16",
		Status: node.StatusOffline, CreatedAt: time.Now(),
	}))
	must(t, r.st.Environments.SetNode(ctx, r.env, "w9"))

	if _, _, err := o.SetPITR(ctx, r.pg.ID, 0); err == nil {
		t.Fatal("off was queued on a node that is not there")
	}
	if m, _ := o.managed.GetByTile(ctx, r.pg.ID); m.PITRDays != r.m.PITRDays {
		t.Errorf("window %d after a refused off, want %d", m.PITRDays, r.m.PITRDays)
	}

	// enabling from off: window and schedule go back too
	r2 := r.m
	r2.PITRDays = 0
	must(t, r.st.ManagedInstances.Update(ctx, r2))
	for _, s := range mustSchedules(t, o, r.data.ID) {
		must(t, o.DeleteBackupSchedule(ctx, s.ID))
	}
	if _, _, err := o.SetPITR(ctx, r.pg.ID, 7); err == nil {
		t.Fatal("on was queued on a node that is not there")
	}
	if m, _ := o.managed.GetByTile(ctx, r.pg.ID); m.PITRDays != 0 {
		t.Errorf("window %d after a refused on, want 0", m.PITRDays)
	}
	if ss := mustSchedules(t, o, r.data.ID); slices.Contains(methodsOf(ss), "base") {
		t.Errorf("base schedule left after a refused on: %v", methodsOf(ss))
	}
	if _, ok := o.pitrState(ctx, r.m.ID); ok {
		t.Error("shipper state left after a refused on")
	}
}

func mustSchedules(t *testing.T, o *Orchestrator, vol string) []BackupSchedule {
	t.Helper()
	ss, err := o.BackupSchedules(context.Background(), vol)
	must(t, err)
	return ss
}

// A branch across nodes dumps on the source instance's node and loads on the
// target's.
func TestSeedFromSliceDumpsOnTheSourceNode(t *testing.T) {
	t.Parallel()
	n := newTwoNodes(t)
	ctx := context.Background()
	o := n.orch
	mk := func(env Environment, slug string, fake *dockerfake.Fake) (inst, slice Tile) {
		var err error
		inst, err = o.CreateManagedTile(ctx, Tile{EnvironmentID: env.ID, Name: "pg-" + slug}, "postgres")
		must(t, err)
		slice, err = o.CreateSliceTile(ctx, env.ID, slug, "shop:"+env.Slug+":"+inst.Slug, "")
		must(t, err)
		m, err := o.managed.GetByTile(ctx, inst.ID)
		must(t, err)
		must(t, n.st.Provisions.Create(ctx, store.Provision{
			ID: uuid.NewString(), TileID: slice.ID, InstanceID: m.ID,
			DBName: "db_" + slug, DBUser: "u_" + slug, DBPassword: "pw", CreatedAt: time.Now(),
		}))
		fake.Containers = append(fake.Containers, docker.Container{
			ID: "c-" + slug, Name: "c-" + slug, State: "running",
			Labels: map[string]string{tile.LabelTile: inst.ID, tile.LabelRole: "replica"},
		})
		fake.ExecOut = "PGDMP"
		return inst, slice
	}
	_, src := mk(n.edge, "src", n.wFake)
	dstInst, dst := mk(n.dev, "dst", n.ctlFake)

	tctx, err := o.withTileNode(ctx, dstInst.ID) // runSeed's context: the target's node
	must(t, err)
	err = o.seedFromSlice(tctx, &jobs.Run{Log: io.Discard}, dst, seedJob{Format: mflow.DumpCustom, Source: seedSource{Kind: "slice", SliceID: src.ID}})
	must(t, err)
	if !sawCall(n.wFake, "ExecStream(c-src, ") {
		t.Errorf("the dump did not run on the source's node: %v", callNames(n.wFake))
	}
	if sawCall(n.ctlFake, "ExecStream(c-src, ") || sawCall(n.wFake, "ExecStream(c-dst, ") {
		t.Errorf("a call went to the wrong node: ctl %v, w1 %v", callNames(n.ctlFake), callNames(n.wFake))
	}
	if !sawCall(n.ctlFake, "ExecStream(c-dst, ") {
		t.Errorf("the load did not run on the target's node: %v", callNames(n.ctlFake))
	}
}

// branch --at restores the source's whole instance, so the caller needs
// write on the instance's stack as well.
func TestBranchAtNeedsWriteOnTheInstancesStack(t *testing.T) {
	t.Parallel()
	r := newBranchRig(t)
	ctx := context.Background()
	other := uuid.NewString()
	must(t, r.st.Stacks.Create(ctx, store.Stack{ID: other, OrgID: r.org, Name: "ops", Slug: "ops", Settings: "{}", CreatedAt: time.Now()}))
	pg, err := r.st.Tiles.Get(ctx, r.pg.ID)
	must(t, err)
	pg.StackID = other
	must(t, r.st.Tiles.Update(ctx, pg))

	by := roleP(r.org, "owner")
	by.Access.Key, by.Access.KeyStack = true, r.stack // a key for the slice's stack only
	_, _, err = r.orch.BranchSlice(ctx, by, r.db.ID, BranchIn{At: "latest"})
	if !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("branch --at with no write on the instance's stack = %v, want not found", err)
	}
}
