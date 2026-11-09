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
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	pgupFacts = "F\tenc\tUTF8\nF\tcollate\ten_US.utf8\nF\tctype\ten_US.utf8\nF\tprov\tc\nF\tsums\toff\nF\tboot\tstackr\n"
	pgupRows  = "C\tpostgres\tpublic.orders\t42\nE\tpostgres\tpublic.big\t9000000\n"
)

// pgupRig is an instance running postgres 17 with a slice, a binding, a
// helper image that answers from a script, and a database that counts rows
// from one.
type pgupRig struct {
	*branchRig
	exit  map[string]int    // a helper step's exit code
	out   map[string]string // a helper step's output
	rows  string            // what the instance's count query answers
	steps func() []string   // the helper steps that ran, in order
	mark  int               // the calls before this one are the rig's own
}

func newPGUpRig(t *testing.T) *pgupRig { t.Helper(); return newPGUpRigOn(t, newBranchRig(t)) }

func newPGUpRigOn(t *testing.T, b *branchRig) *pgupRig {
	t.Helper()
	r := &pgupRig{branchRig: b, exit: map[string]int{}, out: map[string]string{}, rows: pgupRows}
	ctx := context.Background()
	m, err := r.orch.operated.SetMajor(ctx, r.inst, 17)
	must(t, err)
	r.inst = m
	var j *Job
	r.pg, j, err = r.orch.UpdateTile(ctx, r.pg.ID, func(t *Tile) error { t.ImageRef = "postgres:17.5"; return nil })
	must(t, err)
	if j != nil { // its redeploy must be over before the rig is used
		r.wait(t, j.ID)
	}
	_, err = r.orch.engines.Container(r.node(t, ctx), r.pg) // declares the data volume
	must(t, err)
	r.out[mflow.PGStepFacts] = pgupFacts + pgupRows
	r.fake.Helper = func(s docker.ContainerSpec) (string, int, bool) {
		if s.Labels[mflow.LabelPGUpgrade] == "" {
			return "", 0, false
		}
		step := stepOf(s)
		return r.out[step], r.exit[step], true
	}
	r.fake.ExecFn = func(cmd []string) string {
		all := strings.Join(cmd, " ")
		switch {
		case strings.Contains(all, ".get_auth("): // the pooler's proof
			return "1,0,0"
		case strings.Contains(all, "FROM pg_database"):
			return "postgres\n"
		case strings.Contains(all, "query_to_xml"):
			return r.rows
		}
		return ""
	}
	r.steps = func() []string {
		var out []string
		for _, s := range r.fake.Specs {
			if s.Labels[mflow.LabelPGUpgrade] != "" {
				out = append(out, stepOf(s))
			}
		}
		return out
	}
	// whatever is deployed comes up healthy, with an address on the env's network
	r.fake.RunID = "c-up"
	r.fake.Details = map[string]docker.Detail{"c-up": {Running: true, Health: "healthy", Networks: map[string]string{"n": "10.0.0.7"}}}
	r.mark = len(r.fake.Calls())
	return r
}

func stepOf(s docker.ContainerSpec) string {
	for _, e := range s.Env {
		if v, ok := strings.CutPrefix(e, "STEP="); ok {
			return v
		}
	}
	return ""
}

func envOf(s docker.ContainerSpec, key string) string {
	for _, e := range s.Env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			return v
		}
	}
	return ""
}

// helperSpec is the first helper spec that ran step.
func (r *pgupRig) helperSpec(step string) (docker.ContainerSpec, bool) {
	for _, s := range r.fake.Specs {
		if s.Labels[mflow.LabelPGUpgrade] != "" && stepOf(s) == step {
			return s, true
		}
	}
	return docker.ContainerSpec{}, false
}

// begin makes the change as ChangePGMajor does, without the job runner, and
// returns the run for it.
func (r *pgupRig) begin(t *testing.T, to int) *pgupRun {
	t.Helper()
	ctx := context.Background()
	o := r.orch
	_, _, _, err := o.beginPGChange(ctx, roleP(r.org, "member"), r.pg.ID, to)
	must(t, err)
	u := r.run(t, ctx)
	// the candidate's container, once the job deploys it
	r.fake.Edit(func() {
		r.fake.Containers = append(r.fake.Containers, docker.Container{
			ID: "c-cand", Name: "c-cand", State: "running",
			Labels: map[string]string{tile.LabelTile: u.st.Cand, tile.LabelRole: "replica"},
		})
	})
	return u
}

func (r *pgupRig) run(t *testing.T, ctx context.Context) *pgupRun {
	t.Helper()
	o := r.orch
	tl, m, err := o.instanceOf(ctx, r.pg.ID)
	must(t, err)
	sv, err := o.dataVolume(ctx, tl)
	must(t, err)
	st, ok := o.pgupOf(ctx, m.ID)
	if !ok {
		t.Fatal("no change state")
	}
	ctx, err = o.withTileNode(ctx, tl.ID)
	must(t, err)
	_ = ctx
	return &pgupRun{o: o, r: &jobs.Run{Log: io.Discard}, t: tl, m: m, sv: sv, st: st}
}

func TestChangePGMajorRefusals(t *testing.T) {
	t.Parallel()
	r := newPGUpRig(t)
	ctx := context.Background()
	o := r.orch
	writer := roleP(r.org, "member")

	if _, err := o.ChangePGMajor(ctx, roleP(r.org, "viewer"), r.pg.ID, 18); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("viewer = %v", err)
	}
	if _, err := o.ChangePGMajor(ctx, nil, r.pg.ID, 18); err == nil {
		t.Error("no principal changed the version")
	}
	for name, to := range map[string]int{"same": 17, "unknown": 99, "too old": 9} {
		if _, err := o.ChangePGMajor(ctx, writer, r.pg.ID, to); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	// a standby: remove it, change, add it again
	ctx2 := context.Background()
	must(t, r.st.OperatedMembers.Create(ctx2, store.OperatedMember{ID: "mem-1", InstanceID: r.inst.ID, Role: "replica", NodeID: "n", Volume: "v", State: "running", CreatedAt: time.Now()}))
	_, err := o.ChangePGMajor(ctx, writer, r.pg.ID, 18)
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "Remove the standby") {
		t.Errorf("standby: %v", err)
	}
	if len(r.steps()) != 0 {
		t.Error("a helper ran for a refused change")
	}
}

func TestPGUpRoomRefusalGivesTheNumbers(t *testing.T) {
	t.Parallel()
	if err := pgupRoomError("pg", 100<<30, 10<<30); err != nil {
		t.Errorf("plenty: %v", err)
	}
	if err := pgupRoomError("pg", 1<<30, -1); err != nil {
		t.Errorf("unknown size is not judged: %v", err)
	}
	err := pgupRoomError("pg", 5<<30, 10<<30)
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "12.0 GB") || !strings.Contains(err.Error(), "5.0 GB") {
		t.Errorf("err = %v", err)
	}
}

// The helper's first step reads the cluster; the upgrade is the dry run and
// then pg_upgrade in copy mode into the new volume, with the new cluster made
// like the old one.
func TestUpgradeRunsCheckThenCopyModeWithMatchingInitdb(t *testing.T) {
	t.Parallel()
	r := newPGUpRig(t)
	r.out[mflow.PGStepFacts] = strings.Replace(pgupFacts, "sums\toff", "sums\ton", 1) + pgupRows
	u := r.begin(t, 18)
	ctx, _ := r.orch.withTileNode(context.Background(), r.pg.ID)
	for _, step := range []func(context.Context) error{u.stop, u.facts, u.convert} {
		must(t, step(ctx))
	}
	if got := r.steps(); !slices.Equal(got, []string{"facts", "check", "upgrade"}) {
		t.Fatalf("helper steps = %v", got)
	}
	up, _ := r.helperSpec("upgrade")
	if !strings.Contains(strings.Join(up.Cmd, " "), "--copy") || strings.Contains(strings.Join(up.Cmd, " "), "--link") {
		t.Error("pg_upgrade is not run in copy mode")
	}
	flags := envOf(up, "INITDB_FLAGS")
	for _, want := range []string{"--encoding=UTF8", "--lc-collate=en_US.utf8", "--lc-ctype=en_US.utf8", "--data-checksums"} {
		if !strings.Contains(flags, want) {
			t.Errorf("initdb flags %q lack %s", flags, want)
		}
	}
	// two volumes: the old one and a new one, named apart
	if len(up.Volumes) != 2 || !strings.HasSuffix(up.Volumes[0], ":/old") || !strings.HasSuffix(up.Volumes[1], ":/new") || up.Volumes[0] == up.Volumes[1] {
		t.Errorf("volumes = %v", up.Volumes)
	}
	if envOf(up, "OLDDIR") != "/old" || envOf(up, "NEWDIR") != "/new/18/docker" {
		t.Errorf("dirs = %s, %s", envOf(up, "OLDDIR"), envOf(up, "NEWDIR"))
	}
	if chk, _ := r.helperSpec("check"); envOf(chk, "PGU_FLAGS") != "--check" || len(chk.Volumes) != 1 {
		t.Errorf("the dry run is not a check on one volume: %v %v", envOf(chk, "PGU_FLAGS"), chk.Volumes)
	}
	// the source was stopped first: pg_upgrade needs the old cluster down
	if !slices.ContainsFunc(r.fake.Calls(), func(c dockerfake.Call) bool { return c.Method == "Stop" }) {
		t.Error("the instance was not stopped")
	}
}

// 18 turns checksums on by default: an old cluster without them gets the
// flag that keeps them off; one with them, the flag that asks for them; a
// version before 18 needs neither when they are off.
func TestInitdbArgsFollowTheOldCluster(t *testing.T) {
	t.Parallel()
	c := mflow.ClusterFacts{Encoding: "UTF8", Collate: "C", Ctype: "C"}
	if a := strings.Join(c.InitdbArgs(18), " "); !strings.Contains(a, "--no-data-checksums") || strings.Contains(a, " --data-checksums") {
		t.Errorf("18 from no checksums: %s", a)
	}
	if a := strings.Join(c.InitdbArgs(17), " "); strings.Contains(a, "checksums") {
		t.Errorf("17 from no checksums: %s", a)
	}
	c.Checksums = true
	if a := strings.Join(c.InitdbArgs(16), " "); !strings.Contains(a, "--data-checksums") {
		t.Errorf("16 from checksums: %s", a)
	}
}

// A whole upgrade: the instance ends on the new major and image with the
// volumes traded, a new archive epoch, the candidate gone and the old data
// kept as an orphan the rollback can reach.
func TestUpgradeSwapsMajorAndKeepsTheOldVolume(t *testing.T) {
	t.Parallel()
	r := newPGUpRig(t)
	ctx, _ := r.orch.withTileNode(context.Background(), r.pg.ID)
	o := r.orch
	oldVol, err := o.dataVolume(ctx, r.pg)
	must(t, err)
	o.savePITRState(ctx, r.inst.ID, pitrState{ArchSince: time.Now().Add(-48 * time.Hour), LastShippedAt: time.Now()})

	u := r.begin(t, 18)
	cand := u.st.Cand
	must(t, u.run(ctx))

	tl, m, err := o.instanceOf(ctx, r.pg.ID)
	must(t, err)
	if m.Major != 18 || tl.ImageRef != "postgres:18.6" {
		t.Errorf("runs postgres %d on %s; want 18 on postgres:18.6", m.Major, tl.ImageRef)
	}
	nv, _ := o.volumes.Get(ctx, oldVol.ID)
	st, ok := o.pgupOf(ctx, m.ID)
	if !ok || !st.Done || nv.Name == oldVol.Name || st.OldName != oldVol.Name || st.NewName != nv.Name {
		t.Fatalf("state %+v, volume now %s was %s", st, nv.Name, oldVol.Name)
	}
	kept, err := o.volumes.Get(ctx, st.OldVol)
	if err != nil || kept.Name != oldVol.Name || kept.OrphanedAt == nil {
		t.Errorf("the old data is not kept as an orphan: %+v %v", kept, err)
	}
	if _, err := o.tiles.Get(ctx, cand); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("the candidate tile stayed: %v", err)
	}
	// PITR is off here: the reset clears the shipper's state, so no old epoch survives
	if _, ok := o.pitrState(ctx, m.ID); ok {
		t.Error("the archive state survived the change")
	}
	if rb := o.pgupRollback(ctx, m.ID); rb == nil || rb.From != 17 {
		t.Errorf("rollback = %+v", rb)
	}
	v, _ := o.PGVersion(ctx, r.pg.ID)
	if v.Major != 18 || v.Rollback == nil || slices.Contains(v.Majors, 18) || !slices.Contains(v.Majors, 17) {
		t.Errorf("version = %+v", v)
	}
	// the instance was stopped before the conversion, and the candidate before the swap
	if !slices.ContainsFunc(r.fake.Calls(), func(c dockerfake.Call) bool { return c.Method == "Stop" && len(c.Args) > 0 && c.Args[0] == "c-pg" }) {
		t.Error("the instance was not stopped")
	}
}

// With point-in-time recovery on, the change starts a new archive epoch from
// now: the old WAL belongs to another major and no restore reaches back to it.
func TestUpgradeStartsANewArchiveEpoch(t *testing.T) {
	t.Parallel()
	rr := newRestoreRig(t)
	r := newPGUpRigOn(t, rr.branchRig)
	ctx, _ := r.orch.withTileNode(context.Background(), r.pg.ID)
	before, ok := r.orch.pitrState(ctx, r.inst.ID)
	if !ok || !before.ArchSince.Before(time.Now().Add(-time.Hour)) {
		t.Fatalf("setup: %+v %v", before, ok)
	}
	u := r.begin(t, 18)
	must(t, u.run(ctx))
	after, ok := r.orch.pitrState(ctx, r.inst.ID)
	if !ok || after.ArchSince.Before(time.Now().Add(-time.Minute)) || !after.Since.After(time.Now().Add(-time.Minute)) {
		t.Errorf("epoch = %+v, want one starting now", after)
	}
}

// bare is a run with no change behind it: what a dry run is.
func (r *pgupRig) bare(t *testing.T) (*pgupRun, context.Context) {
	t.Helper()
	ctx, err := r.orch.withTileNode(context.Background(), r.pg.ID)
	must(t, err)
	tl, m, err := r.orch.instanceOf(ctx, r.pg.ID)
	must(t, err)
	sv, err := r.orch.dataVolume(ctx, tl)
	must(t, err)
	return &pgupRun{o: r.orch, r: &jobs.Run{Log: io.Discard}, t: tl, m: m, sv: sv}, ctx
}

func (r *pgupRig) called(method string) []dockerfake.Call {
	var out []dockerfake.Call
	for _, c := range r.fake.Calls()[r.mark:] {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// The dry run of a running instance checks a copy: the instance is not
// stopped, the copy is a base backup unpacked into a scratch volume that is
// removed after, and nothing but pg_upgrade --check runs on it. The answer is
// remembered and no change is begun.
func TestCheckOnlyLeavesTheInstanceAlone(t *testing.T) {
	t.Parallel()
	r := newPGUpRig(t)
	u, ctx := r.bare(t)
	must(t, u.check(ctx, 18))

	if got := r.steps(); !slices.Equal(got, []string{"facts", "check"}) {
		t.Errorf("helper steps = %v", got)
	}
	if len(r.called("Stop")) != 0 || len(r.called("Create")) > 2 { // the helpers' two
		t.Errorf("the dry run stopped or deployed something: %v", r.called("Stop"))
	}
	if bb := r.called("ExecStream"); len(bb) != 1 || !slices.Contains(bb[0].Args, "pg_basebackup") {
		t.Errorf("no base backup copy: %v", bb)
	}
	if v := r.called("CreateVolume"); len(v) == 0 || len(r.called("RemoveVolume")) == 0 {
		t.Error("the scratch volume was not made and removed")
	}
	chk, _ := r.helperSpec("check")
	if envOf(chk, "OLDDIR") != "/old" || len(chk.Volumes) != 1 || strings.Contains(chk.Volumes[0], u.sv.Name) {
		t.Errorf("the check did not run on the scratch copy: %v %s", chk.Volumes, envOf(chk, "OLDDIR"))
	}
	if envOf(chk, "COUNTS") != "" {
		t.Error("the dry run counted rows")
	}
	v, err := r.orch.PGVersion(ctx, r.pg.ID)
	must(t, err)
	if v.Check == nil || !v.Check.OK || v.Check.To != 18 || v.Check.NeedBytes < 0 {
		t.Errorf("check = %+v", v.Check)
	}
	if _, ok := r.orch.pgupOf(ctx, r.inst.ID); ok {
		t.Error("a dry run began a change")
	}
}

// A refused dry run fails with pg_upgrade's reasons and remembers them.
func TestCheckOnlyRefusalIsRemembered(t *testing.T) {
	t.Parallel()
	r := newPGUpRig(t)
	r.exit[mflow.PGStepCheck] = 1
	r.out[mflow.PGStepCheck] = "Your installation contains the \"reg*\" data type in user tables.\n"
	u, ctx := r.bare(t)
	err := u.check(ctx, 18)
	if err == nil || !strings.Contains(err.Error(), "reg*") {
		t.Fatalf("err = %v", err)
	}
	v, _ := r.orch.PGVersion(ctx, r.pg.ID)
	if v.Check == nil || v.Check.OK || !strings.Contains(v.Check.Detail, "reg*") {
		t.Errorf("check = %+v", v.Check)
	}
}

// Rows that do not match, or a pg_upgrade that fails, stop before the swap and
// put everything back: the instance runs again on its own volume and major,
// the candidate and its volume are gone and no change is left behind.
func TestFailureBeforeTheSwapChangesNothing(t *testing.T) {
	t.Parallel()
	for name, fail := range map[string]func(r *pgupRig){
		"rows differ":     func(r *pgupRig) { r.rows = "C\tpostgres\tpublic.orders\t41\nE\tpostgres\tpublic.big\t9000000\n" },
		"table missing":   func(r *pgupRig) { r.rows = "C\tpostgres\tpublic.orders\t42\n" },
		"big table moved": func(r *pgupRig) { r.rows = "C\tpostgres\tpublic.orders\t42\nE\tpostgres\tpublic.big\t8000000\n" },
		"pg_upgrade":      func(r *pgupRig) { r.exit[mflow.PGStepUpgrade] = 1; r.out[mflow.PGStepUpgrade] = "could not copy file" },
		"dry run":         func(r *pgupRig) { r.exit[mflow.PGStepCheck] = 1 },
		"facts":           func(r *pgupRig) { r.exit[mflow.PGStepFacts] = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newPGUpRig(t)
			r.fake.StopExits = map[string]bool{"c-pg": true}
			fail(r)
			ctx, _ := r.orch.withTileNode(context.Background(), r.pg.ID)
			before, err := r.orch.dataVolume(ctx, r.pg)
			must(t, err)
			u := r.begin(t, 18)
			cand := u.st.Cand
			if err := u.run(ctx); err == nil {
				t.Fatal("the change went through")
			}
			tl, m, _ := r.orch.instanceOf(ctx, r.pg.ID)
			after, _ := r.orch.dataVolume(ctx, tl)
			if m.Major != 17 || tl.ImageRef != "postgres:17.5" || after.Name != before.Name {
				t.Errorf("changed: %d %s %s", m.Major, tl.ImageRef, after.Name)
			}
			if _, err := r.orch.tiles.Get(ctx, cand); !errors.Is(err, errs.ErrNotFound) {
				t.Errorf("the candidate stayed: %v", err)
			}
			if _, ok := r.orch.pgupOf(ctx, m.ID); ok {
				t.Error("the change state stayed")
			}
			if len(r.called("Start")) == 0 {
				t.Error("the instance was not started again")
			}
		})
	}
}

// A downgrade is a dump with the newer pg_dumpall into a fresh volume made by
// the older major (no dry run: nothing to ask of pg_upgrade). Big tables are
// not compared by size, which a load does not keep.
func TestDowngradeDumpsAndLoads(t *testing.T) {
	t.Parallel()
	r := newPGUpRig(t)
	r.rows = "C\tpostgres\tpublic.orders\t42\nE\tpostgres\tpublic.big\t123\n"
	ctx, _ := r.orch.withTileNode(context.Background(), r.pg.ID)
	u := r.begin(t, 16)
	must(t, u.run(ctx))

	if got := r.steps(); !slices.Equal(got, []string{"facts", "dump"}) {
		t.Fatalf("helper steps = %v", got)
	}
	d, _ := r.helperSpec("dump")
	if !strings.Contains(strings.Join(d.Cmd, " "), "pg_dumpall") || envOf(d, "OLDMAJOR") != "17" || envOf(d, "NEWMAJOR") != "16" {
		t.Errorf("dump spec: %v %s %s", d.Cmd[:1], envOf(d, "OLDMAJOR"), envOf(d, "NEWMAJOR"))
	}
	if envOf(d, "NEWDIR") != "/new" { // 16 keeps its data at the volume's root
		t.Errorf("new dir = %s", envOf(d, "NEWDIR"))
	}
	tl, m, _ := r.orch.instanceOf(ctx, r.pg.ID)
	if m.Major != 16 || tl.ImageRef != "postgres:16.15" {
		t.Errorf("runs %d on %s", m.Major, tl.ImageRef)
	}
	if rb := r.orch.pgupRollback(ctx, m.ID); rb == nil || rb.From != 17 {
		t.Errorf("rollback = %+v", rb)
	}
}

// A load that fails says what broke and changes nothing.
func TestDowngradeLoadFailureChangesNothing(t *testing.T) {
	t.Parallel()
	r := newPGUpRig(t)
	r.exit[mflow.PGStepDump] = 3
	r.out[mflow.PGStepDump] = `psql:<stdin>:12: ERROR:  unrecognized configuration parameter "foo"`
	ctx, _ := r.orch.withTileNode(context.Background(), r.pg.ID)
	u := r.begin(t, 16)
	err := u.run(ctx)
	if err == nil || !strings.Contains(err.Error(), `unrecognized configuration parameter "foo"`) {
		t.Fatalf("err = %v", err)
	}
	if _, m, _ := r.orch.instanceOf(ctx, r.pg.ID); m.Major != 17 {
		t.Errorf("major = %d", m.Major)
	}
}

// A job that dies after any step resumes at the next: no helper runs twice,
// and the change ends the same.
func TestResumeAfterACrashAtEachStep(t *testing.T) {
	t.Parallel()
	names := []string{"stop the instance", "read the old cluster", "convert the data", "start the new cluster",
		"verify the new cluster", "update extensions and statistics", "swap in the new data", "start the instance on it", "start a new archive"}
	for _, crash := range names {
		t.Run(crash, func(t *testing.T) {
			t.Parallel()
			r := newPGUpRig(t)
			ctx, _ := r.orch.withTileNode(context.Background(), r.pg.ID)
			oldVol, _ := r.orch.dataVolume(ctx, r.pg)
			u := r.begin(t, 18)
			u.after = func(step string) error {
				if step == crash {
					return context.Canceled
				}
				return nil
			}
			if err := u.run(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("the crash: %v", err)
			}
			if _, ok := r.orch.pgupOf(ctx, r.inst.ID); !ok {
				t.Fatal("a crash dropped the state")
			}
			must(t, r.run(t, ctx).run(ctx))

			if got := r.steps(); !slices.Equal(got, []string{"facts", "check", "upgrade"}) {
				t.Errorf("helper steps = %v: one ran twice or was skipped", got)
			}
			tl, m, _ := r.orch.instanceOf(ctx, r.pg.ID)
			nv, _ := r.orch.dataVolume(ctx, tl)
			st, _ := r.orch.pgupOf(ctx, m.ID)
			if m.Major != 18 || !st.Done || nv.Name == oldVol.Name || st.OldName != oldVol.Name {
				t.Errorf("major %d, state %+v, volume %s was %s", m.Major, st, nv.Name, oldVol.Name)
			}
		})
	}
}

// A swap that committed but whose cursor was lost is not done again: that
// would hand the old data back.
func TestSwapIsNotRepeated(t *testing.T) {
	t.Parallel()
	r := newPGUpRig(t)
	ctx, _ := r.orch.withTileNode(context.Background(), r.pg.ID)
	oldVol, _ := r.orch.dataVolume(ctx, r.pg)
	u := r.begin(t, 18)
	u.after = func(step string) error {
		if step == "swap in the new data" {
			return context.Canceled
		}
		return nil
	}
	if err := u.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	swapped, _ := r.orch.volumes.Get(ctx, oldVol.ID)
	st, _ := r.orch.pgupOf(ctx, r.inst.ID)
	st.Step, st.OldName = pgupSwapAt, "" // the cursor save was lost
	must(t, r.orch.setPGUp(ctx, r.inst.ID, st))
	must(t, r.run(t, ctx).run(ctx))

	again, _ := r.orch.volumes.Get(ctx, oldVol.ID)
	done, _ := r.orch.pgupOf(ctx, r.inst.ID)
	if again.Name != swapped.Name || again.Name == oldVol.Name || done.OldName != oldVol.Name {
		t.Errorf("volume %s (swapped to %s, was %s), old = %s", again.Name, swapped.Name, oldVol.Name, done.OldName)
	}
}

// A node without the room for a second copy refuses before anything is made,
// with the numbers.
func TestChangeRefusedWithoutRoom(t *testing.T) {
	t.Parallel()
	r := newPGUpRig(t)
	ctx := context.Background()
	v, err := r.orch.dataVolume(ctx, r.pg)
	must(t, err)
	r.fake.Edit(func() { r.fake.Volumes = append(r.fake.Volumes, docker.VolumeInfo{Name: v.Name, SizeBytes: 1 << 60}) })
	_, err = r.orch.ChangePGMajor(ctx, roleP(r.org, "member"), r.pg.ID, 18)
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "needs") {
		t.Errorf("err = %v", err)
	}
	if _, err := r.orch.CheckPGUpgrade(ctx, roleP(r.org, "member"), r.pg.ID, 18); err == nil {
		t.Error("a dry run was queued without room for its copy")
	}
	if _, ok := r.orch.pgupOf(ctx, r.inst.ID); ok {
		t.Error("a refused change left state")
	}
}

// The old data is kept for a week and the rollback reaches it until then;
// after, the sweep ends it.
func TestRollbackWithinAWeekAndCleanupAfter(t *testing.T) {
	t.Parallel()
	r := newPGUpRig(t)
	o := r.orch
	ctx, _ := o.withTileNode(context.Background(), r.pg.ID)
	writer := roleP(r.org, "member")
	oldVol, _ := o.dataVolume(ctx, r.pg)
	if _, err := o.RollbackPGUpgrade(ctx, writer, r.pg.ID); err == nil || !strings.Contains(err.Error(), "no version change") {
		t.Errorf("nothing to roll back: %v", err)
	}
	u := r.begin(t, 18)
	must(t, u.run(ctx))
	st, _ := o.pgupOf(ctx, r.inst.ID)
	newVol, _ := o.dataVolume(ctx, r.pg)

	if _, err := o.RollbackPGUpgrade(ctx, roleP(r.org, "viewer"), r.pg.ID); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("viewer = %v", err)
	}
	j, err := o.RollbackPGUpgrade(ctx, writer, r.pg.ID)
	must(t, err)
	if got := r.wait(t, j.ID); got.State != "done" {
		t.Fatalf("rollback job %s: %s", got.State, got.Error)
	}
	tl, m, _ := o.instanceOf(ctx, r.pg.ID)
	back, _ := o.dataVolume(ctx, tl)
	if m.Major != 17 || tl.ImageRef != "postgres:17.5" || back.Name != oldVol.Name {
		t.Errorf("after rollback: %d %s on %s, want 17 postgres:17.5 on %s", m.Major, tl.ImageRef, back.Name, oldVol.Name)
	}
	lost, _ := o.volumes.Get(ctx, st.OldVol)
	if lost.Name != newVol.Name || lost.OrphanedAt == nil {
		t.Errorf("the new data should be the orphan now: %+v", lost)
	}
	if _, ok := o.pgupOf(ctx, m.ID); ok {
		t.Error("the record stayed after the rollback")
	}

	// a week on, there is nothing to roll back to, and the sweep ends the volume
	r2 := newPGUpRig(t)
	o2 := r2.orch
	ctx2, _ := o2.withTileNode(context.Background(), r2.pg.ID)
	must(t, r2.begin(t, 18).run(ctx2))
	st2, _ := o2.pgupOf(ctx2, r2.inst.ID)
	st2.At = time.Now().Add(-6 * 24 * time.Hour)
	must(t, o2.setPGUp(ctx2, r2.inst.ID, st2))
	o2.sweepPGUpgrades(ctx2)
	if _, err := o2.volumes.Get(ctx2, st2.OldVol); err != nil {
		t.Errorf("the old volume went at six days: %v", err)
	}
	st2.At = time.Now().Add(-8 * 24 * time.Hour)
	must(t, o2.setPGUp(ctx2, r2.inst.ID, st2))
	if _, err := o2.RollbackPGUpgrade(ctx2, roleP(r2.org, "member"), r2.pg.ID); err == nil {
		t.Error("rolled back after a week")
	}
	o2.sweepPGUpgrades(ctx2)
	if _, err := o2.volumes.Get(ctx2, st2.OldVol); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("the old volume stayed after a week: %v", err)
	}
	if rm := r2.called("RemoveVolume"); !slices.ContainsFunc(rm, func(c dockerfake.Call) bool { return slices.Contains(c.Args, st2.OldName) }) {
		t.Error("the old Docker volume was not removed")
	}
	if _, ok := o2.pgupOf(ctx2, r2.inst.ID); ok {
		t.Error("the record stayed")
	}
}

// Through the job runner: a change that fails at its first step ends the job
// failed, with the reason, and leaves nothing behind.
func TestChangePGMajorJobFailsCleanly(t *testing.T) {
	t.Parallel()
	r := newPGUpRig(t)
	r.exit[mflow.PGStepFacts] = 1
	r.out[mflow.PGStepFacts] = "pg_ctl: could not start server"
	ctx := context.Background()
	j, err := r.orch.ChangePGMajor(ctx, roleP(r.org, "member"), r.pg.ID, 18)
	must(t, err)
	got := r.wait(t, j.ID)
	if got.State != "failed" || !strings.Contains(got.Error, "could not start server") {
		t.Errorf("job %s: %s", got.State, got.Error)
	}
	ts, _ := r.orch.tiles.List(ctx, r.env)
	for _, tl := range ts {
		if strings.Contains(tl.Slug, "-pg18-") {
			t.Errorf("the candidate %s stayed", tl.Slug)
		}
	}
	if _, ok := r.orch.pgupOf(ctx, r.inst.ID); ok {
		t.Error("the change state stayed")
	}
}
