package service

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A postgres major change, up or down. The instance's data is never converted
// in place: a new volume is made by the target major (pg_upgrade in copy mode
// for an upgrade, a dump and load for a downgrade), checked as a private
// candidate tile, and swapped in the way a point-in-time promote swaps. The
// old volume stays for pgupKeep, and the change can be rolled back until then.
//
// pg_upgrade needs the old cluster shut down, so the instance is stopped from
// the start of the conversion to the end of the swap: that is the downtime. A
// failure before the swap restarts it and changes nothing.

const (
	kindPGUpgrade  jobs.Kind = "pg-upgrade"
	kindPGRollback jobs.Kind = "pg-upgrade-rollback"

	// pgupKey is an instance's change in flight, or the one it can still roll
	// back: pgup.<instance id>. pgupCheckKey is its last dry run.
	pgupKey      = "pgup."
	pgupCheckKey = "pgupcheck."
	pgupKeep     = 7 * 24 * time.Hour
	// pgupWait bounds one helper run.
	// ponytail: twelve hours for any size; a copy that takes longer needs this
	// to be a setting.
	pgupWait = 12 * time.Hour
)

// PGCheck is the last dry run of a major change: whether pg_upgrade would
// take the data, in its own words when it would not.
type PGCheck struct {
	To        int       `json:"to"`
	OK        bool      `json:"ok"`
	Detail    string    `json:"detail"`
	NeedBytes int64     `json:"need_bytes"` // the free disk a change needs
	At        time.Time `json:"at"`
}

// PGRollback says the instance's previous major is kept and until when.
type PGRollback struct {
	From  int       `json:"from"`
	Until time.Time `json:"until"`
}

// pgUp is a change's state and cursor: Step is how many steps are done, so a
// job that dies resumes at the next. Once Done it is the record a rollback
// reads, and Prev the record of an earlier change that it replaces.
type pgUp struct {
	From, To           int
	FromImage, ToImage string
	Cand               string // the candidate tile
	Step               int
	WasUp              bool // the instance ran when the change began
	Facts              mflow.ClusterFacts
	Counts             string // the old cluster's rows, counted
	Done               bool
	// Swapped is written in the transaction of the swap itself: from then on
	// the new data is authoritative and the run only ever goes forward.
	Swapped bool
	// RollingBack is set while a rollback runs, so no standby is added meanwhile.
	RollingBack      bool
	At               time.Time
	OldVol           string // the volume row that holds the old data once Done
	OldName, NewName string // the Docker volumes of the old and the new data
	Prev             *pgUp
}

func (o *Orchestrator) pgupOf(ctx context.Context, instanceID string) (pgUp, bool) {
	var st pgUp
	v, ok, err := o.store.Settings.Get(ctx, pgupKey+instanceID)
	if err != nil || !ok || json.Unmarshal([]byte(v), &st) != nil {
		return pgUp{}, false
	}
	return st, true
}

func (o *Orchestrator) setPGUp(ctx context.Context, instanceID string, st pgUp) error {
	b, _ := json.Marshal(st)
	return o.store.Settings.Set(ctx, pgupKey+instanceID, string(b))
}

// pgupRollback is the rollback the instance still has, if any.
func (o *Orchestrator) pgupRollback(ctx context.Context, instanceID string) *PGRollback {
	if st, ok := o.pgupOf(ctx, instanceID); ok && st.Done && time.Since(st.At) < pgupKeep {
		return &PGRollback{From: st.From, Until: st.At.Add(pgupKeep)}
	}
	return nil
}

// lastPGCheck is the instance's last dry run.
func (o *Orchestrator) lastPGCheck(ctx context.Context, instanceID string) *PGCheck {
	var c PGCheck
	if v, ok, err := o.store.Settings.Get(ctx, pgupCheckKey+instanceID); err == nil && ok && json.Unmarshal([]byte(v), &c) == nil {
		return &c
	}
	return nil
}

// pgChange is a change the caller may start: the instance and where it goes.
type pgChange struct {
	t        Tile
	m        OperatedInstance
	sv       store.Volume
	from, to int
}

// pgChangeOf checks that the caller may change tileID's major to `to` and that
// nothing stands in the way (pgInstanceOf).
func (o *Orchestrator) pgChangeOf(ctx context.Context, by *Principal, tileID string, to int) (pgChange, error) {
	c, err := o.pgInstanceOf(ctx, by, tileID)
	if err != nil {
		return c, err
	}
	c.to = to
	return c, mflow.PGChangeCheck(c.from, to, time.Now())
}

// pgInstanceOf is the instance of tileID when the caller may write to its
// stack and nothing stands in the way: its node up, no fence, no standby.
func (o *Orchestrator) pgInstanceOf(ctx context.Context, by *Principal, tileID string) (pgChange, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return pgChange{}, err
	}
	if m.Engine != "postgres" {
		return pgChange{}, errs.Invalidf("engine", "%s has no version to change; only postgres does", m.Engine)
	}
	st, err := o.stacks.Get(ctx, t.StackID)
	if err != nil {
		return pgChange{}, err
	}
	if err := mayWrite(by, st); err != nil {
		return pgChange{}, err
	}
	if err := o.gateTile(ctx, t.ID, true); err != nil {
		return pgChange{}, err
	}
	if err := o.mayStartTile(ctx, t); err != nil {
		return pgChange{}, err
	}
	if err := o.noStandbyForChange(ctx, t, m); err != nil {
		return pgChange{}, err
	}
	sv, err := o.dataVolume(ctx, t)
	if err != nil {
		return pgChange{}, errs.Conflictf("%s has no data volume yet; deploy it first", t.Slug)
	}
	return pgChange{t: t, m: m, sv: sv, from: m.Major}, nil
}

// noStandbyForChange: a standby follows the primary's data, which a change
// replaces by a new cluster it cannot follow.
// ponytail: refused for now; remove the standbys, change the version, add them
// again. Rebuilding them from the new primary is the upgrade of this.
func (o *Orchestrator) noStandbyForChange(ctx context.Context, t Tile, m OperatedInstance) error {
	ms, err := o.members.List(ctx, m.ID)
	if err != nil {
		return err
	}
	if opmember.HasReplica(ms) {
		return errs.Conflictf("%s has a standby. Remove the standby, change the version, then add the standby again.", t.Slug)
	}
	return nil
}

// pgupRoomError is nil when free bytes hold the new volume of a size-byte
// instance, else the refusal with the numbers.
func pgupRoomError(slug string, free uint64, size int64) error {
	need := mflow.PGUpgradeNeed(size)
	if size < 0 || int64(free) >= need {
		return nil
	}
	return errs.Conflictf("changing the version of %s needs %s free on its node (its data %s, copied into a new volume, and a fifth over) and it has %s",
		slug, gb(uint64(need)), gb(uint64(size)), gb(free))
}

func (o *Orchestrator) pgupRoom(ctx context.Context, c pgChange) error {
	id := o.volumeNode(ctx, c.sv.ID)
	if id == "" {
		return nil
	}
	free, _ := o.diskOf(ctx, id)
	if free == 0 {
		return nil // the node does not report its disk: not judged, as a deploy's check
	}
	return pgupRoomError(c.t.Slug, free, o.volumes.SizeBytes(ctx, c.sv))
}

// pgupLocks is the lock set of the change: the instance and what is bound to
// it, its data volume and the candidate.
func (o *Orchestrator) pgupLocks(ctx context.Context, c pgChange, extra ...string) ([]string, error) {
	lock, err := o.instanceLocks(ctx, c.sv)
	if err != nil {
		return nil, err
	}
	return append(append(lock, "volume:"+c.sv.ID), extra...), nil
}

type pgUpgradeJob struct {
	TileID string `json:"tile_id"`
	To     int    `json:"to"`
	Check  bool   `json:"check,omitempty"`
}

// CheckPGUpgrade queues a dry run of moving the instance to major `to`: the
// new version's own pg_upgrade --check, on a copy when the instance runs, so
// the instance is neither stopped nor changed. The answer is PGVersion.Check.
func (o *Orchestrator) CheckPGUpgrade(ctx context.Context, by *Principal, tileID string, to int) (Job, error) {
	c, err := o.pgChangeOf(ctx, by, tileID, to)
	if err != nil {
		return Job{}, err
	}
	if err := o.pgupRoom(ctx, c); err != nil {
		return Job{}, err
	}
	lock, err := o.pgupLocks(ctx, c)
	if err != nil {
		return Job{}, err
	}
	return o.enqueue(ctx, kindPGUpgrade, pgUpgradeJob{TileID: c.t.ID, To: to, Check: true}, lock...)
}

// ChangePGMajor queues the move of the instance to major `to`, up or down
// (job). A change already in flight for the same major is resumed.
func (o *Orchestrator) ChangePGMajor(ctx context.Context, by *Principal, tileID string, to int) (Job, error) {
	c, lock, undo, err := o.beginPGChange(ctx, by, tileID, to)
	if err != nil {
		return Job{}, err
	}
	j, err := o.enqueue(ctx, kindPGUpgrade, pgUpgradeJob{TileID: c.t.ID, To: to}, lock...)
	if err != nil {
		undo()
		return Job{}, err
	}
	o.auditBy(ctx, by, securitylog.InstanceMajor, c.t.ID, fmt.Sprintf("%d -> %d, %s", c.from, to, pgDirection(c.from, to)))
	return j, nil
}

// beginPGChange checks the change and makes what the job runs on: the
// candidate tile and the state (or, for a change in flight, only the locks).
// undo takes them away again when the job cannot be queued.
func (o *Orchestrator) beginPGChange(ctx context.Context, by *Principal, tileID string, to int) (c pgChange, lock []string, undo func(), err error) {
	undo = func() {}
	if c, err = o.pgInstanceOf(ctx, by, tileID); err != nil {
		return
	}
	st, have := o.pgupOf(ctx, c.m.ID)
	if have && !st.Done {
		if st.To != to {
			err = errs.Conflictf("a change to postgres %d is in progress; wait for it to end", st.To)
			return
		}
		lock, err = o.pgupLocks(ctx, c, st.Cand)
		return
	}
	c.to = to
	if err = mflow.PGChangeCheck(c.from, to, time.Now()); err != nil {
		return
	}
	if err = o.pgupRoom(ctx, c); err != nil {
		return
	}
	cand, err := o.newCandidateAs(ctx, pitrSource{tile: c.t, inst: c.m},
		slugWith(c.t.Slug, "-pg"+strconv.Itoa(to)+"-"+time.Now().UTC().Format("20060102-1504")), mustPGImage(to), to, nil)
	if err != nil {
		return
	}
	undo = func() {
		o.forgetCandidate(ctx, cand)
		_ = o.store.Settings.Delete(ctx, pgupKey+c.m.ID)
		if have {
			_ = o.setPGUp(ctx, c.m.ID, st)
		}
	}
	n := pgUp{From: c.from, To: to, FromImage: cmp.Or(c.t.ImageRef, mflow.Engines["postgres"].Definition().Image), ToImage: mustPGImage(to), Cand: cand.ID}
	if have {
		n.Prev = &st // the change it replaces, still rollbackable until this one is done
	}
	if lock, err = o.pgupLocks(ctx, c, cand.ID); err == nil {
		err = o.setPGUp(ctx, c.m.ID, n)
	}
	if err != nil {
		undo()
	}
	return
}

func mustPGImage(major int) string {
	r, _ := mflow.PGReleaseOf(major)
	return r.Image()
}

func pgDirection(from, to int) string {
	if to < from {
		return "downgrade"
	}
	return "upgrade"
}

func (o *Orchestrator) tileRunning(ctx context.Context, t Tile) bool {
	cs, err := o.tiles.Replicas(ctx, t)
	return err == nil && slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" })
}

// pgupRun is one run of the job: the instance, its volume, and the state.
type pgupRun struct {
	o     *Orchestrator
	r     *jobs.Run
	t     Tile
	m     OperatedInstance
	sv    store.Volume
	st    pgUp
	after func(step string) error // a test's way to stop the run after a step
}

func (o *Orchestrator) runPGUpgrade(ctx context.Context, r *jobs.Run, p pgUpgradeJob) error {
	t, m, err := o.instanceOf(ctx, p.TileID)
	if err != nil {
		return err
	}
	if ctx, err = o.withTileNode(ctx, t.ID); err != nil {
		return err
	}
	o.ensureMesh(ctx)
	sv, err := o.dataVolume(ctx, t)
	if err != nil {
		return err
	}
	u := &pgupRun{o: o, r: r, t: t, m: m, sv: sv}
	if p.Check {
		return u.check(ctx, p.To)
	}
	st, ok := o.pgupOf(ctx, m.ID)
	if !ok || st.Done || st.To != p.To {
		return errs.Conflictf("there is no change to postgres %d to run; start one again", p.To)
	}
	u.st = st
	return u.run(ctx)
}

// helper runs one step of the helper image to its end and returns its output.
func (u *pgupRun) helper(ctx context.Context, run mflow.UpgradeRun, tail int) (string, error) {
	run.Instance, run.Admin, run.From = u.m.ID, u.m.AdminUser, u.st.From
	if run.From == 0 {
		run.From = u.m.Major
	}
	spec, err := run.Spec()
	if err != nil {
		return "", err
	}
	node, _ := docker.NodeFrom(ctx) // the run's own node: the data is there
	out, code, err := u.o.oneShotFor(ctx, node, spec, nil, pgupWait, tail)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return out, fmt.Errorf("%s exited %d:\n%s", run.Step, code, strings.TrimSpace(out))
	}
	return out, nil
}

// ---- the dry run ----

func (u *pgupRun) check(ctx context.Context, to int) error {
	o := u.o
	u.st.From, u.st.To = u.m.Major, to
	if err := o.pgupRoom(ctx, pgChange{t: u.t, m: u.m, sv: u.sv, from: u.m.Major, to: to}); err != nil {
		return err
	}
	res, filled := PGCheck{To: to, At: time.Now().UTC(), NeedBytes: mflow.PGUpgradeNeed(max(o.volumes.SizeBytes(ctx, u.sv), 0))}, false
	defer func() {
		if filled {
			b, _ := json.Marshal(res)
			_ = o.store.Settings.Set(context.WithoutCancel(ctx), pgupCheckKey+u.m.ID, string(b))
		}
	}()
	vol, scratch := u.sv.Name, false
	if o.tileRunning(ctx, u.t) {
		// the instance runs: check a copy, so it is neither stopped nor touched
		name := "stackr-pgupcheck-" + u.m.ID[:8]
		_ = o.docker.RemoveVolume(ctx, name)
		if err := o.docker.CreateVolume(ctx, name, "local", nil, map[string]string{mflow.LabelPGUpgrade: u.m.ID}); err != nil {
			return err
		}
		defer func() { _ = o.docker.RemoveVolume(context.WithoutCancel(ctx), name) }()
		if err := u.copyTo(ctx, name); err != nil {
			return err
		}
		vol, scratch = name, true
	}
	out, err := u.helper(ctx, mflow.UpgradeRun{Name: "stackr-pgup-facts-" + u.m.ID[:8], Step: mflow.PGStepFacts, OldVol: vol, To: to, ScratchOld: scratch}, 2000)
	if err != nil {
		return err
	}
	facts, err := mflow.ParseFacts(out)
	if err != nil {
		return err
	}
	filled = true
	if to < u.m.Major {
		// a downgrade is a dump and a load: nothing to ask of pg_upgrade
		res.OK = true
		res.Detail = "the data is dumped by postgres " + strconv.Itoa(u.m.Major) + " and loaded into postgres " + strconv.Itoa(to) + "; a load that fails changes nothing"
		return nil
	}
	out, err = u.helper(ctx, mflow.UpgradeRun{Name: "stackr-pgup-check-" + u.m.ID[:8], Step: mflow.PGStepCheck, OldVol: vol, To: to, Facts: facts, ScratchOld: scratch}, 60)
	if err != nil {
		res.Detail = err.Error()
		_, _ = fmt.Fprintf(u.r.Log, "pg_upgrade --check refused:\n%s\n", res.Detail)
		return fmt.Errorf("%w: %s", errCheckRefused, firstLines(res.Detail, 12))
	}
	res.OK, res.Detail = true, "postgres "+strconv.Itoa(to)+" can take this data"
	_, _ = fmt.Fprintf(u.r.Log, "%s\n", res.Detail)
	return nil
}

// errCheckRefused is a dry run that ran and said no: its answer is stored,
// and the job fails with the reasons.
var errCheckRefused = errors.New("the data cannot be upgraded")

func firstLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

// copyTo takes a consistent copy of the running instance into volume name:
// a base backup over the container's socket (the WAL it needs included),
// unpacked there.
// ponytail: no tablespaces (pg_basebackup cannot send them to stdout).
func (u *pgupRun) copyTo(ctx context.Context, name string) error {
	o := u.o
	cs, err := o.tiles.Replicas(ctx, u.t)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(cs, func(c docker.Container) bool { return c.State == "running" })
	if i < 0 {
		return errs.Conflictf("%s is not running", u.t.Slug)
	}
	_, _ = fmt.Fprintf(u.r.Log, "copying %s to check the copy\n", u.t.Slug)
	out, wait, err := o.docker.ExecStream(ctx, cs[i].ID, []string{"pg_basebackup", "-h", "/var/run/postgresql", "-U", u.m.AdminUser, "-D", "-", "-Ft", "-X", "fetch"}, nil)
	if err != nil {
		return err
	}
	uerr := o.docker.UntarVolume(ctx, name, out)
	if werr := wait(); werr != nil {
		return fmt.Errorf("copy the data: %w", werr)
	}
	return uerr
}

// ---- the change ----

type pgupStep struct {
	name string
	do   func(ctx context.Context) error
}

// swapAt is the index of the swap in run: a failure up to and including it
// leaves the instance as it was.
const pgupSwapAt = 6

func (u *pgupRun) run(ctx context.Context) error {
	steps := []pgupStep{
		{"stop the instance", u.stop},
		{"read the old cluster", u.facts},
		{"convert the data", u.convert},
		{"start the new cluster", u.start},
		{"verify the new cluster", u.verify},
		{"update extensions and statistics", u.finishUp},
		{"swap in the new data", u.swap},
		{"start the instance on it", u.restart},
		{"start a new archive", u.settle},
	}
	for i := u.st.Step; i < len(steps); i++ {
		_, _ = fmt.Fprintf(u.r.Log, "%s\n", steps[i].name)
		if err := steps[i].do(ctx); err != nil {
			// a crash or a shutdown is no failure: the cursor resumes it
			if i <= pgupSwapAt && !u.st.Swapped && ctx.Err() == nil {
				u.abort(ctx, err)
			}
			return fmt.Errorf("%s: %w", steps[i].name, err)
		}
		u.st.Step = i + 1
		if err := u.o.setPGUp(context.WithoutCancel(ctx), u.m.ID, u.st); err != nil {
			return err
		}
		if u.after != nil {
			if err := u.after(steps[i].name); err != nil {
				return err
			}
		}
	}
	return u.done(ctx)
}

func (u *pgupRun) candidate(ctx context.Context) (Tile, error) { return u.o.tiles.Get(ctx, u.st.Cand) }

func (u *pgupRun) stop(ctx context.Context) error {
	if !u.o.tileRunning(ctx, u.t) {
		return nil
	}
	u.st.WasUp = true
	if err := u.o.setPGUp(ctx, u.m.ID, u.st); err != nil { // before the stop: a crash must still restart it
		return err
	}
	return u.o.container.Stop(ctx, u.t, u.r.Log)
}

func (u *pgupRun) facts(ctx context.Context) error {
	out, err := u.helper(ctx, mflow.UpgradeRun{Name: "stackr-pgup-facts-" + u.m.ID[:8], Step: mflow.PGStepFacts, OldVol: u.sv.Name, To: u.st.To, Counts: true}, 20000)
	if err != nil {
		return err
	}
	if u.st.Facts, err = mflow.ParseFacts(out); err != nil {
		return err
	}
	u.st.Counts = out
	return nil
}

func (u *pgupRun) candidateVolume(ctx context.Context, cand Tile) (store.Volume, error) {
	cm, err := u.o.operated.GetByTile(ctx, cand.ID)
	if err != nil {
		return store.Volume{}, err
	}
	cv, _, err := u.o.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: cand.EnvironmentID}, cand.Slug+"-data", &cm.ID)
	if err != nil {
		return cv, err
	}
	_, err = u.o.volumes.Ensure(ctx, cv)
	return cv, err
}

func (u *pgupRun) convert(ctx context.Context) error {
	cand, err := u.candidate(ctx)
	if err != nil {
		return err
	}
	cv, err := u.candidateVolume(ctx, cand)
	if err != nil {
		return err
	}
	run := mflow.UpgradeRun{Name: "stackr-pgup-run-" + u.m.ID[:8], OldVol: u.sv.Name, NewVol: cv.Name, To: u.st.To, Facts: u.st.Facts}
	if u.st.To < u.st.From {
		_, _ = fmt.Fprintf(u.r.Log, "dumping postgres %d and loading it into postgres %d\n", u.st.From, u.st.To)
		run.Step = mflow.PGStepDump
	} else {
		// the dry run first, on the data as it stands now that it is stopped
		chk := run
		chk.Name, chk.Step, chk.NewVol = "stackr-pgup-check-"+u.m.ID[:8], mflow.PGStepCheck, ""
		if _, err := u.helper(ctx, chk, 60); err != nil {
			return fmt.Errorf("pg_upgrade --check refused: %w", err)
		}
		_, _ = fmt.Fprintf(u.r.Log, "running pg_upgrade (copy mode): postgres %d to %d\n", u.st.From, u.st.To)
		run.Step = mflow.PGStepUpgrade
	}
	out, err := u.helper(ctx, run, 40)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(u.r.Log, "%s\n", strings.TrimSpace(out))
	return nil
}

func (u *pgupRun) start(ctx context.Context) error {
	cand, err := u.candidate(ctx)
	if err != nil {
		return err
	}
	if err := u.o.deploy.Redeploy(ctx, cand.ID, u.r.Log, u.r.Swap); err != nil {
		return fmt.Errorf("the new cluster did not start: %w%s", err, u.o.logTail(ctx, cand))
	}
	return nil
}

// verify: every login the stack uses works on the new cluster, and every
// table has the rows the old one had.
func (u *pgupRun) verify(ctx context.Context) error {
	o := u.o
	cand, err := u.candidate(ctx)
	if err != nil {
		return err
	}
	ps, err := o.operated.ByInstance(ctx, u.m.ID)
	if err != nil {
		return err
	}
	var logins []mflow.PGLogin
	for _, p := range ps {
		logins = append(logins, mflow.PGLogin{User: p.DBUser, Password: p.DBPassword, DB: p.DBName})
		bs, err := o.operated.Bindings(ctx, p.ID)
		if err != nil {
			return err
		}
		for _, b := range bs {
			logins = append(logins, mflow.PGLogin{User: b.DBUser, Password: b.DBPassword, DB: p.DBName})
		}
	}
	if err := o.engines.PGLogins(ctx, cand, logins); err != nil {
		return err
	}
	after, err := o.engines.PGCounts(ctx, cand, u.r.Log)
	if err != nil {
		return err
	}
	if diff := mflow.CompareCounts(u.st.Counts, after); len(diff) > 0 {
		if len(diff) > 10 {
			diff = append(diff[:10], fmt.Sprintf("and %d more", len(diff)-10))
		}
		return fmt.Errorf("the new cluster does not hold what the old one did: %s", strings.Join(diff, "; "))
	}
	_, _ = fmt.Fprintf(u.r.Log, "logins work and every table has the rows it had\n")
	return nil
}

func (u *pgupRun) finishUp(ctx context.Context) error {
	cand, err := u.candidate(ctx)
	if err != nil {
		return err
	}
	return u.o.engines.PGAfterChange(ctx, cand, u.r.Log)
}

// swap trades the volumes, and the image and major the instance runs, in one
// transaction. Only a swap orphans the candidate's data volume while its tile
// lives, which is how a re-run knows it already happened.
func (u *pgupRun) swap(ctx context.Context) error {
	o := u.o
	if u.st.Swapped { // an earlier run swapped, and its cursor save was lost
		return nil
	}
	cand, err := u.candidate(ctx)
	if err != nil {
		return err
	}
	cv, err := o.dataVolume(ctx, cand) // read, never declared: declaring re-adopts an orphan
	if err != nil {
		return err
	}
	// the topology holds still from the last look to the swap: no standby
	// added, no fence set, in between
	mu := o.topoLock(u.m.ID)
	mu.Lock()
	defer mu.Unlock()
	if err := o.pgChangeSafe(ctx, u.t, u.m); err != nil {
		return err
	}
	if o.tileRunning(ctx, cand) {
		if err := o.container.Stop(ctx, cand, u.r.Log); err != nil {
			return err
		}
	}
	if err := u.r.Swap(); err != nil {
		return err
	}
	sv, err := o.volumes.Get(ctx, u.sv.ID)
	if err != nil {
		return err
	}
	u.st.OldVol, u.st.OldName, u.st.NewName, u.st.Swapped = cv.ID, sv.Name, cv.Name, true
	st := u.st
	err = o.swapMajor(ctx, sv, cv, u.t.ID, u.m.ID, u.st.ToImage, u.st.To, func(tx store.Tx) error {
		b, _ := json.Marshal(st)
		return tx.Settings.Set(ctx, pgupKey+u.m.ID, string(b))
	})
	if err != nil {
		u.st.Swapped = false
	}
	return err
}

// pgChangeSafe is what must hold for the swap: no standby (it would follow
// data that no longer exists) and a primary that is not fenced. Asked under
// the topology lock, right before the swap.
func (o *Orchestrator) pgChangeSafe(ctx context.Context, t Tile, m OperatedInstance) error {
	if err := o.noStandbyForChange(ctx, t, m); err != nil {
		return err
	}
	return o.mayStartTile(ctx, t)
}

// swapMajor is swapData with the instance's image and major, and the next
// archive epoch, all or nothing; extra runs in the same transaction.
func (o *Orchestrator) swapMajor(ctx context.Context, sv, cv store.Volume, tileID, instanceID, image string, major int, extra func(store.Tx) error) error {
	sv.Name, cv.Name = cv.Name, sv.Name
	now := time.Now().UTC()
	cv.OrphanedAt = &now
	return o.store.Tx(ctx, func(tx store.Tx) error {
		t, err := tx.Tiles.Get(ctx, tileID)
		if err != nil {
			return err
		}
		m, err := tx.OperatedInstances.Get(ctx, instanceID)
		if err != nil {
			return err
		}
		t.ImageRef, m.Major = image, major
		m.ArchiveEpoch++ // the old major's WAL and the new cluster's never share an archive
		for _, err := range []error{tx.Volumes.Update(ctx, sv), tx.Volumes.Update(ctx, cv), tx.Tiles.Update(ctx, t), tx.OperatedInstances.Update(ctx, m)} {
			if err != nil {
				return err
			}
		}
		if extra != nil {
			return extra(tx)
		}
		return nil
	})
}

func (u *pgupRun) restart(ctx context.Context) error {
	o := u.o
	if err := o.deploy.Redeploy(ctx, u.t.ID, u.r.Log, nil); err != nil {
		o.downAfterFailedDeploy(ctx, u.t.ID, err)
		return err
	}
	if err := o.afterDeploy(ctx, []string{u.t.ID}, false); err != nil {
		return err
	}
	return o.engines.Replay(ctx, u.t, u.r.Log)
}

// settle starts a new archive epoch: the old WAL is of another major and the
// new cluster has its own history, so no restore reaches back across.
func (u *pgupRun) settle(ctx context.Context) error {
	m, err := u.o.operated.GetByTile(ctx, u.t.ID)
	if err != nil {
		return err
	}
	if err := u.o.pitrReset(ctx, m); err != nil {
		return err
	}
	if m.PITRDays > 0 {
		_, err = u.o.BackupNow(ctx, u.sv.ID, "", "base", "")
	}
	return err
}

// done records the change as rollbackable, takes the candidate's tile away
// (its volume, now the old data, stays orphaned) and ends the replaced
// change's own old volume.
func (u *pgupRun) done(ctx context.Context) error {
	o := u.o
	ctx = context.WithoutCancel(ctx)
	if cand, err := u.candidate(ctx); err == nil {
		o.dropCandidate(ctx, cand, false, u.r.Log)
	}
	if u.st.Prev != nil {
		o.dropKept(ctx, *u.st.Prev, u.r.Log)
		u.st.Prev = nil
	}
	_ = o.store.Settings.Delete(ctx, pgupCheckKey+u.m.ID) // it was of the data as it was
	u.st.Done, u.st.At = true, time.Now().UTC()
	if err := o.setPGUp(ctx, u.m.ID, u.st); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(u.r.Log, "%s runs postgres %d now. The old data is kept until %s; roll back before then to return to it.\n",
		u.t.Slug, u.st.To, u.st.At.Add(pgupKeep).Format(time.RFC3339))
	return nil
}

// abort puts everything back as it was before the swap: the instance runs
// again if it did, the candidate and its volume go, the state is the one
// before (a replaced rollback record) or none.
func (u *pgupRun) abort(ctx context.Context, cause error) {
	o := u.o
	if u.st.Swapped { // the old volume is the rollback now: nothing here may touch it
		return
	}
	ctx = tearingDown(context.WithoutCancel(ctx))
	_, _ = fmt.Fprintf(u.r.Log, "stopping without a change: %v\n", cause)
	if u.st.WasUp && !o.tileRunning(ctx, u.t) {
		if err := o.mayStartTile(ctx, u.t); err != nil {
			_, _ = fmt.Fprintf(u.r.Log, "warning: %s stays down: %v\n", u.t.Slug, err)
		} else if err := o.container.Start(ctx, u.t, u.r.Log); err != nil {
			_, _ = fmt.Fprintf(u.r.Log, "warning: %s did not restart: %v\n", u.t.Slug, err)
		}
	}
	if cand, err := u.candidate(ctx); err == nil {
		o.dropCandidate(ctx, cand, true, u.r.Log)
	}
	if u.st.Prev != nil {
		_ = o.setPGUp(ctx, u.m.ID, *u.st.Prev)
	} else {
		_ = o.store.Settings.Delete(ctx, pgupKey+u.m.ID)
	}
}

// dropKept deletes the old volume a finished change kept.
func (o *Orchestrator) dropKept(ctx context.Context, st pgUp, log io.Writer) {
	v, err := o.volumes.Get(ctx, st.OldVol)
	if errors.Is(err, errs.ErrNotFound) {
		return
	}
	if err == nil {
		err = o.volumes.Delete(ctx, v, nil)
	}
	if err != nil {
		_, _ = fmt.Fprintf(log, "warning: the old data of postgres %d stays: %v\n", st.From, err)
	}
}

// ---- rollback ----

// RollbackPGUpgrade queues the return to the major the instance ran before
// its last change, with the data it had then: what was written since is lost.
// Possible for a week after the change (job).
func (o *Orchestrator) RollbackPGUpgrade(ctx context.Context, by *Principal, tileID string) (Job, error) {
	c, err := o.pgInstanceOf(ctx, by, tileID)
	if err != nil {
		return Job{}, err
	}
	if o.pgupRollback(ctx, c.m.ID) == nil {
		return Job{}, errs.Conflictf("%s has no version change to roll back; the old data is kept for 7 days", c.t.Slug)
	}
	lock, err := o.pgupLocks(ctx, c)
	if err != nil {
		return Job{}, err
	}
	j, err := o.enqueue(ctx, kindPGRollback, pgUpgradeJob{TileID: c.t.ID}, lock...)
	if err == nil {
		o.auditBy(ctx, by, securitylog.InstanceMajor, c.t.ID, "rollback")
	}
	return j, err
}

func (o *Orchestrator) runPGRollback(ctx context.Context, r *jobs.Run, p pgUpgradeJob) error {
	t, m, err := o.instanceOf(ctx, p.TileID)
	if err != nil {
		return err
	}
	if ctx, err = o.withTileNode(ctx, t.ID); err != nil {
		return err
	}
	o.ensureMesh(ctx)
	st, ok := o.pgupOf(ctx, m.ID)
	if !ok || !st.Done || time.Since(st.At) >= pgupKeep {
		return errs.Conflictf("%s has no version change to roll back", t.Slug)
	}
	sv, err := o.dataVolume(ctx, t)
	if err != nil {
		return err
	}
	ov, err := o.volumes.Get(ctx, st.OldVol)
	if err != nil {
		return errs.Conflictf("the old data of %s is gone; it cannot be rolled back", t.Slug)
	}
	if !st.RollingBack {
		st.RollingBack = true
		if err := o.setPGUp(ctx, m.ID, st); err != nil {
			return err
		}
	}
	if sv.Name != st.OldName { // not yet swapped back (a re-run finds it done)
		mu := o.topoLock(m.ID)
		mu.Lock()
		defer mu.Unlock()
		if err := o.pgChangeSafe(ctx, t, m); err != nil {
			st.RollingBack = false
			_ = o.setPGUp(ctx, m.ID, st)
			return err
		}
		_, _ = fmt.Fprintf(r.Log, "stop the instance\n")
		was := o.tileRunning(ctx, t)
		if was {
			if err := o.container.Stop(ctx, t, r.Log); err != nil {
				return err
			}
		}
		_, _ = fmt.Fprintf(r.Log, "swap the old data back in\n")
		if err := r.Swap(); err != nil {
			return err
		}
		if err := o.swapMajor(ctx, sv, ov, t.ID, m.ID, st.FromImage, st.From, nil); err != nil {
			if was {
				_ = o.container.Start(context.WithoutCancel(ctx), t, r.Log)
			}
			return err
		}
	}
	_, _ = fmt.Fprintf(r.Log, "start the instance on it\n")
	if err := o.deploy.Redeploy(ctx, t.ID, r.Log, nil); err != nil {
		o.downAfterFailedDeploy(ctx, t.ID, err)
		return err
	}
	if err := o.afterDeploy(ctx, []string{t.ID}, false); err != nil {
		return err
	}
	if err := o.engines.Replay(ctx, t, r.Log); err != nil {
		return err
	}
	m, err = o.operated.GetByTile(ctx, t.ID)
	if err != nil {
		return err
	}
	if err := o.pitrReset(ctx, m); err != nil {
		return err
	}
	if m.PITRDays > 0 {
		if _, err := o.BackupNow(ctx, sv.ID, "", "base", ""); err != nil {
			return err
		}
	}
	_ = o.store.Settings.Delete(ctx, pgupKey+m.ID)
	_, _ = fmt.Fprintf(r.Log, "%s runs postgres %d again, on the data it had before the change.\n", t.Slug, st.From)
	return nil
}

// ---- retention ----

// sweepPGUpgrades ends the old data a change kept once pgupKeep has passed,
// and the records of instances that are gone. A change that never finished
// keeps its record: it is resumed, or aborted, by its job.
func (o *Orchestrator) sweepPGUpgrades(ctx context.Context) {
	rows, err := o.store.Settings.ListPrefix(ctx, pgupKey)
	if err != nil {
		return
	}
	for k, v := range rows {
		var st pgUp
		if json.Unmarshal([]byte(v), &st) != nil || !st.Done {
			continue
		}
		if _, err := o.operated.Get(ctx, strings.TrimPrefix(k, pgupKey)); errors.Is(err, errs.ErrNotFound) || time.Since(st.At) >= pgupKeep {
			o.dropKept(ctx, st, slogWriter{})
			_ = o.store.Settings.Delete(ctx, k)
		}
	}
}

// slogWriter sends a helper's warnings to the panel's log.
type slogWriter struct{}

func (slogWriter) Write(p []byte) (int, error) {
	slog.Warn("pg upgrade", "msg", strings.TrimSpace(string(p)))
	return len(p), nil
}
