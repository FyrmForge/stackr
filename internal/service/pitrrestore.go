package service

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	fbackup "github.com/FyrmForge/stackr/internal/service/internal/flow/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Point-in-time restore, the panel's half: restore an instance to a time T
// into a new instance tile (a candidate), copy a slice out of it (branch
// --at), or promote the candidate over the source. The archive reading is
// flow/backup's (FillVolume); the rules about which base and which time are
// leaf/backup's (pitrrestore.go).

const (
	kindRestoreInstance jobs.Kind = "restore-instance"
	kindPromoteRestore  jobs.Kind = "promote-restore"

	// candKey marks a tile as a restored candidate: pitrcand.<tile id>.
	candKey = "pitrcand."

	// pitrRecoveryWait bounds the replay after the candidate starts.
	// ponytail: one hour for any size; a base that replays longer needs this
	// to be a setting.
	pitrRecoveryWait = time.Hour
	pitrRecoveryPoll = 2 * time.Second
	// pitrTempMaxAge is how long a temporary candidate (a branch --at's)
	// may outlive its job before the hourly sweep deletes it.
	pitrTempMaxAge = 24 * time.Hour
)

// candidate is the settings row behind a restored tile.
type candidate struct {
	Shipped bool      `json:"shipped,omitempty"` // its timeline history is in the archive
	Vol     string    `json:"vol,omitempty"`     // the Docker volume the source takes at the swap
	Source  string    `json:"source"`            // the instance tile restored
	At      string    `json:"at"`                // RFC3339 UTC, or latest
	Temp    bool      `json:"temp"`              // a branch --at's scratch copy: gone when its job ends
	Created time.Time `json:"created"`           // for the sweep
}

func (o *Orchestrator) candidateOf(ctx context.Context, tileID string) (candidate, bool) {
	var c candidate
	v, ok, err := o.store.Settings.Get(ctx, candKey+tileID)
	if err != nil || !ok || json.Unmarshal([]byte(v), &c) != nil {
		return candidate{}, false
	}
	return c, true
}

func (o *Orchestrator) setCandidate(ctx context.Context, tileID string, c candidate) error {
	b, _ := json.Marshal(c)
	return o.store.Settings.Set(ctx, candKey+tileID, string(b))
}

// RestoreCandidate is the instance a restored tile was cut from and the time
// it was restored to; ok is false for any other tile.
func (o *Orchestrator) RestoreCandidate(ctx context.Context, tileID string) (source Tile, at string, ok bool) {
	c, ok := o.candidateOf(ctx, tileID)
	if !ok || c.Temp {
		return Tile{}, "", false
	}
	src, err := o.tiles.Get(ctx, c.Source)
	if err != nil {
		return Tile{}, "", false
	}
	return src, c.At, true
}

// pitrSource is an instance's archive as a restore reads it.
type pitrSource struct {
	tile Tile
	inst OperatedInstance
	vol  store.Volume
	org  string
	dest store.BackupDest
	root string
}

func (o *Orchestrator) pitrSourceOf(ctx context.Context, tileID string) (pitrSource, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return pitrSource{}, err
	}
	if mflow.Engines[m.Engine].Definition().ArchiveDir == "" {
		return pitrSource{}, errs.Invalidf("engine", "%s has no point-in-time recovery", m.Engine)
	}
	if m.PITRDays <= 0 {
		return pitrSource{}, errs.Conflictf("point-in-time recovery is off for %s; turn it on first (it only restores to times after that)", t.Slug)
	}
	v, err := o.dataVolume(ctx, t)
	if err != nil {
		return pitrSource{}, errs.Conflictf("%s has no data volume yet; deploy it first", t.Slug)
	}
	org, err := o.volumeOrg(ctx, v)
	if err != nil {
		return pitrSource{}, err
	}
	dest, err := o.pitrDest(ctx, org, v)
	if err != nil {
		return pitrSource{}, err
	}
	return pitrSource{t, m, v, org, dest, backup.PITREpochRoot(org, v.ID, m.ArchiveEpoch)}, nil
}

// pitrPlan checks that the instance can be restored to atStr and reads what
// the restore needs: the base and WAL, with the time's bounds enforced
// (branch: and its margin from now) and the node's disk judged. The caller
// holds the archive against pruning, or only wants the answer.
func (o *Orchestrator) pitrPlan(ctx context.Context, src pitrSource, atStr string, branch bool) (plan backup.Plan, at time.Time, latest bool, err error) {
	at, latest, err = backup.ParseAt(atStr)
	if err != nil {
		return plan, at, latest, err
	}
	bases, keys, lin, err := o.pitrArchive(ctx, src.dest, src.root, src.inst.ID)
	if err != nil {
		return plan, at, latest, fmt.Errorf("read the archive: %w", err)
	}
	from, ok := backup.Restorable(bases, keys, lin)
	if !ok {
		return plan, at, latest, errs.Conflictf("%s has no base backup that can be restored yet: one can, once it has finished and its WAL has been shipped", src.tile.Slug)
	}
	st, _ := o.pitrState(ctx, src.inst.ID)
	to := st.restorableTo()
	if to.IsZero() {
		return plan, at, latest, errs.Conflictf("%s has not shipped any WAL yet", src.tile.Slug)
	}
	if to.Before(from) && !latest { // the newest base is past the archive's reach yet
		return plan, at, latest, errs.Conflictf("%s has no base backup that can be restored yet: one can, once it has finished and its WAL has been shipped", src.tile.Slug)
	}
	if err := backup.CheckAt(at, latest, from, to, time.Now(), branch); err != nil {
		return plan, at, latest, err
	}
	if plan, err = backup.PickBase(bases, keys, lin, st.Marks, at, latest); err != nil {
		return plan, at, latest, err
	}
	return plan, at, latest, o.restoreRoom(ctx, src, plan)
}

// restoreRoom refuses a restore the node's disk cannot hold, with the
// numbers: 1.2 x (the base as it unpacks + the WAL it replays).
func (o *Orchestrator) restoreRoom(ctx context.Context, src pitrSource, plan backup.Plan) error {
	id := o.volumeNode(ctx, src.vol.ID)
	if id == "" {
		return nil
	}
	free, _ := o.diskOf(ctx, id)
	if free == 0 {
		return nil // the node does not report its disk: not judged, as a deploy's check
	}
	base := max(o.volumes.SizeBytes(ctx, src.vol), plan.Base.SizeBytes)
	return roomError(src.tile.Slug, free, base, plan.Segments)
}

// roomError is nil when free bytes hold 1.2 x (the base and segments of WAL),
// else the refusal with the numbers.
func roomError(slug string, free uint64, base int64, segments int) error {
	need := backup.DiskNeed(base, segments)
	if int64(free) >= need {
		return nil
	}
	return errs.Conflictf("restoring %s needs %s free on its node (1.2 x (base %s + WAL %s)) and it has %s",
		slug, gb(uint64(need)), gb(uint64(base)), gb(uint64(segments)*backup.SegmentBytes), gb(free))
}

// restoredSlug names a candidate <src>-restored-YYYYMMDD-HHMM; a temporary
// one adds a random tail so two branches at once never meet. The name, with
// the "-data" volume declared on it, fits volume.MaxName: a source that would
// overflow is cut and takes 4 hex of the hash of the whole, so long names
// sharing a prefix stay apart.
func restoredSlug(src string, label time.Time, temp bool) string {
	tail := "-restored-" + label.UTC().Format("20060102-1504")
	if temp {
		tail += "-" + randomSuffix(4)
	}
	return slugWith(src, tail)
}

// slugWith is src + tail, with src cut (and hashed) when the name with its
// "-data" volume would pass volume.MaxName.
func slugWith(src, tail string) string {
	if room := volume.MaxName - len("-data") - len(tail); len(src) > room {
		h := sha256.Sum256([]byte(src))
		src = strings.TrimRight(src[:room-5], "-") + "-" + hex.EncodeToString(h[:2])
	}
	return src + tail
}

// newCandidate makes the candidate's tile row and instance: an operated
// postgres in the source's env with its image and sizing, archiving off,
// the source's admin credentials (the restored cluster holds their hashes),
// no slices, no expiry. Nothing runs.
func (o *Orchestrator) newCandidate(ctx context.Context, src pitrSource, label time.Time, atStr string, temp bool) (Tile, error) {
	return o.newCandidateAs(ctx, src, restoredSlug(src.tile.Slug, label, temp),
		cmp.Or(src.tile.ImageRef, mflow.Engines[src.inst.Engine].Definition().Image), src.inst.Major,
		&candidate{Source: src.tile.ID, At: atStr, Temp: temp, Created: time.Now().UTC()})
}

// newCandidateAs is newCandidate with the slug, the image and its major
// given; mark is the settings row that makes the tile a restored copy (nil =
// none: a major change's tile is no copy to promote).
func (o *Orchestrator) newCandidateAs(ctx context.Context, src pitrSource, slug, image string, major int, mark *candidate) (Tile, error) {
	if _, err := o.tiles.GetBySlug(ctx, src.tile.EnvironmentID, slug); err == nil {
		return Tile{}, errs.Conflictf("%s already exists; delete it first or pick another time", slug)
	} else if !errors.Is(err, errs.ErrNotFound) {
		return Tile{}, err
	}
	row := Tile{
		EnvironmentID: src.tile.EnvironmentID,
		Name:          slug,
		Slug:          slug,
		ImageRef:      image,
		MemLimitMB:    src.tile.MemLimitMB,
		CPULimit:      src.tile.CPULimit,
		ShmSizeMB:     src.tile.ShmSizeMB,
	}
	t, _, err := o.createOperated(ctx, row, src.inst.Engine, "", -1)
	if err != nil {
		return Tile{}, err
	}
	m, err := o.operated.GetByTile(ctx, t.ID)
	if err == nil {
		m.PITRDays, m.AdminUser, m.AdminPassword = 0, src.inst.AdminUser, src.inst.AdminPassword
		// the restored stackr.conf is the source's: the copy keeps its settings
		m.Settings, m.Major = src.inst.Settings, major
		err = o.store.OperatedInstances.Update(ctx, m)
	}
	if err == nil && mark != nil {
		err = o.setCandidate(ctx, t.ID, *mark)
	}
	if err != nil {
		_ = o.store.Tiles.Delete(ctx, t.ID) // nothing ran: the rows go
		return Tile{}, err
	}
	return t, nil
}

// normAt is the time as the candidate and the log record it.
func normAt(at time.Time, latest bool) string {
	if latest {
		return "latest"
	}
	return at.UTC().Format(time.RFC3339)
}

// RestoreInstance restores instance tileID to at (RFC3339 with an offset, or
// latest) into a new instance tile next to it, and queues the job that fills
// and starts it. The source is not touched; the candidate is kept until it is
// promoted or deleted. The caller needs tile.write on the source's stack.
func (o *Orchestrator) RestoreInstance(ctx context.Context, by *Principal, tileID, atStr string) (Tile, Job, error) {
	src, err := o.pitrSourceOf(ctx, tileID)
	if err != nil {
		return Tile{}, Job{}, err
	}
	st, err := o.stacks.Get(ctx, src.tile.StackID)
	if err != nil {
		return Tile{}, Job{}, err
	}
	if err := mayWrite(by, st); err != nil {
		return Tile{}, Job{}, err
	}
	if err := o.gateTile(ctx, src.tile.ID, true); err != nil {
		return Tile{}, Job{}, err
	}
	_, at, latest, err := o.pitrPlan(ctx, src, atStr, false)
	if err != nil {
		return Tile{}, Job{}, err
	}
	label := at
	if latest {
		label = time.Now()
	}
	// ponytail: a config-managed stack's next sync removes this tile like any
	// the file does not declare; promote or delete it first.
	cand, err := o.newCandidate(ctx, src, label, normAt(at, latest), false)
	if err != nil {
		return Tile{}, Job{}, err
	}
	j, err := o.enqueue(ctx, kindRestoreInstance, restoreInstanceJob{SourceID: src.tile.ID, CandidateID: cand.ID, At: normAt(at, latest)}, cand.ID)
	if err != nil {
		o.forgetCandidate(ctx, cand)
		return Tile{}, Job{}, err
	}
	o.auditBy(ctx, by, securitylog.InstanceRestore, cand.ID, fmt.Sprintf("%s -> %s, at=%s", src.tile.Slug, cand.Slug, normAt(at, latest)))
	return cand, j, nil
}

// forgetCandidate removes a candidate whose job never ran: nothing started,
// so its rows go.
func (o *Orchestrator) forgetCandidate(ctx context.Context, cand Tile) {
	if m, err := o.operated.GetByTile(ctx, cand.ID); err == nil {
		_ = o.store.OperatedInstances.Delete(ctx, m.ID)
	}
	_ = o.store.Tiles.Delete(ctx, cand.ID)
	_ = o.store.Settings.Delete(ctx, candKey+cand.ID)
}

type restoreInstanceJob struct {
	SourceID    string `json:"source_id"`
	CandidateID string `json:"candidate_id"`
	At          string `json:"at"`
}

func (o *Orchestrator) runRestoreInstance(ctx context.Context, r *jobs.Run, p restoreInstanceJob) error {
	cand, err := o.tiles.Get(ctx, p.CandidateID)
	if err != nil {
		return err
	}
	if ctx, err = o.withTileNode(ctx, cand.ID); err != nil {
		return err
	}
	o.ensureMesh(ctx)
	if err := o.restoreInto(ctx, r, p.SourceID, cand, p.At); err != nil {
		o.dropCandidate(ctx, cand, true, r.Log)
		return err
	}
	_, _ = fmt.Fprintf(r.Log, "%s is up at %s; promote it over %s, or delete it\n", cand.Slug, p.At, p.SourceID)
	return nil
}

// restoreInto fills candidate cand's data volume from the source's archive,
// starts it and waits until the replay is over and it is a primary. The
// archive is held against pruning from the plan to the last byte read.
func (o *Orchestrator) restoreInto(ctx context.Context, r *jobs.Run, srcID string, cand Tile, atStr string) error {
	src, err := o.pitrSourceOf(ctx, srcID)
	if err != nil {
		return err
	}
	release := o.backup.Hold(src.root)
	defer release()
	plan, at, latest, err := o.pitrPlan(ctx, src, atStr, false)
	if err != nil {
		return err
	}
	if err := o.fillCandidate(ctx, r, src, cand, plan, at, latest); err != nil {
		return err
	}
	release() // everything is read; a prune may run
	if err := o.startAndAwait(ctx, r, src, cand, plan, at, latest, true); err != nil {
		return err
	}
	return o.engines.Settle(ctx, cand)
}

// noCommitAfterTarget is the one failure the restore retries: postgres 13+
// stops when the WAL runs out before the target time, which for an archive
// known to be complete up to the target only means no commit came after it.
func noCommitAfterTarget(err error) bool {
	return err != nil && strings.Contains(err.Error(), "recovery ended before configured recovery target was reached")
}

// startAndAwait starts the filled candidate and waits for the replay to end.
// If postgres stops because no commit came after the target (the range end
// guarantees every WAL file up to it is shipped), it refills the volume
// from the same plan without the target and starts it once more: replaying
// all of it is the state at the target. The timeline setting is the same
// 'latest' both times, so the retry reads the same history. down opens the
// down problem when the first deploy leaves nothing running.
func (o *Orchestrator) startAndAwait(ctx context.Context, r *jobs.Run, src pitrSource, cand Tile, plan backup.Plan, at time.Time, latest, down bool) error {
	err := o.startOnce(ctx, r, cand, down)
	if latest || !noCommitAfterTarget(err) {
		return err
	}
	_, _ = fmt.Fprintf(r.Log, "no commit after %s in the archive: replaying all of it, the same state as at %s\n", normAt(at, false), normAt(at, false))
	if cs, rerr := o.tiles.Replicas(ctx, cand); rerr == nil && len(cs) > 0 {
		if serr := o.container.Stop(ctx, cand, r.Log); serr != nil {
			return errors.Join(err, serr)
		}
	}
	release := o.backup.Hold(src.root)
	defer release()
	if ferr := o.fillCandidate(ctx, r, src, cand, plan, at, true); ferr != nil {
		return errors.Join(err, ferr)
	}
	release()
	if err := o.startOnce(ctx, r, cand, down); err != nil {
		return err
	}
	o.clearProblem(ctx, downAlertKey(cand.ID)) // the first start's failure opened it
	return nil
}

func (o *Orchestrator) startOnce(ctx context.Context, r *jobs.Run, cand Tile, down bool) error {
	// a replica that fails the gate is removed; its last output only reaches
	// the job log, so the retry looks there for postgres's reason
	var out strings.Builder
	if err := o.deploy.Redeploy(ctx, cand.ID, io.MultiWriter(r.Log, &out), r.Swap); err != nil {
		// the readiness check gives up after a minute; a replay may take longer
		cs, rerr := o.tiles.Replicas(ctx, cand)
		if rerr != nil || !slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" }) {
			if down {
				o.downAfterFailedDeploy(ctx, cand.ID, err)
			}
			if noCommitAfterTarget(errors.New(out.String())) {
				err = fmt.Errorf("%w: recovery ended before configured recovery target was reached", err)
			}
			return fmt.Errorf("start the restored copy: %w%s", err, o.logTail(ctx, cand))
		}
		_, _ = fmt.Fprintf(r.Log, "still replaying (%v)\n", err)
	}
	return o.awaitRecovery(ctx, cand, r.Log)
}

func (o *Orchestrator) fillCandidate(ctx context.Context, r *jobs.Run, src pitrSource, cand Tile, plan backup.Plan, at time.Time, latest bool) error {
	m, err := o.operated.GetByTile(ctx, cand.ID)
	if err != nil {
		return err
	}
	v, _, err := o.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: cand.EnvironmentID}, cand.Slug+"-data", &m.ID)
	if err != nil {
		return err
	}
	if _, err := o.volumes.Ensure(ctx, v); err != nil {
		return err
	}
	when := at
	if latest {
		when = time.Time{}
	}
	conf, walDir, prefix, err := o.engines.RecoveryConf(ctx, cand, when)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(r.Log, "restoring %s to %s from the base of %s\n", src.tile.Slug, normAt(at, latest), plan.Base.FinishedAt.UTC().Format(time.RFC3339))
	return o.backup.FillVolume(ctx, fbackup.Replay{Dest: src.dest, Plan: plan, Target: v, WalDir: walDir, Prefix: prefix, Conf: conf}, r.Log)
}

// recoveryFailed are the lines postgres logs when a replay cannot finish: it
// exits, and the container restarts into the same wall.
var recoveryFailed = []string{
	"recovery ended before configured recovery target was reached",
	"before consistent recovery point",
	"could not locate required checkpoint record",
	"could not locate a valid checkpoint record",
	"invalid checkpoint record",
	"requested timeline",
}

// awaitRecovery polls until the candidate is out of recovery, fails fast on a
// replay postgres gave up on, and gives up after pitrRecoveryWait.
func (o *Orchestrator) awaitRecovery(ctx context.Context, cand Tile, log io.Writer) error {
	_, _ = fmt.Fprintf(log, "waiting for %s to finish replaying WAL\n", cand.Slug)
	deadline := time.Now().Add(pitrRecoveryWait)
	for n := 0; ; n++ {
		rec, err := o.engines.Recovering(ctx, cand)
		if err == nil && !rec {
			return nil
		}
		if err != nil && n%3 == 2 { // not answering: is it dead for good?
			if tail := o.logTail(ctx, cand); slices.ContainsFunc(recoveryFailed, func(s string) bool { return strings.Contains(tail, s) }) {
				hint := ""
				if strings.Contains(tail, "before configured recovery target") {
					// a quiet database's idle tail is retried before this; this is the retry failing too
					hint = " (the archive ends before that time; pick an earlier one, or latest)"
				}
				return fmt.Errorf("the replay stopped%s%s", hint, tail)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s was still replaying after %s%s", cand.Slug, pitrRecoveryWait, o.logTail(ctx, cand))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pitrRecoveryPoll):
		}
	}
}

// logTail is the end of the candidate's log as a suffix for an error.
func (o *Orchestrator) logTail(ctx context.Context, t Tile) string {
	out, err := o.Logs(ctx, t.ID, "", 15)
	if err != nil || strings.TrimSpace(out) == "" {
		return ""
	}
	return ":\n" + strings.TrimSpace(out)
}

// dropCandidate removes a candidate's tile and instance; hard also deletes
// its volumes now (a scratch copy has no use for an orphan's retention).
// Best effort: what stays is logged.
func (o *Orchestrator) dropCandidate(ctx context.Context, cand Tile, hard bool, log io.Writer) {
	ctx = tearingDown(context.WithoutCancel(ctx))
	var vs []store.Volume
	if m, err := o.operated.GetByTile(ctx, cand.ID); err == nil {
		all, _ := o.volumes.List(ctx, volume.Scope{Kind: "env", ID: cand.EnvironmentID})
		for _, v := range all {
			if v.InstanceID != nil && *v.InstanceID == m.ID {
				vs = append(vs, v)
			}
		}
	}
	if e, err := o.envs.Get(ctx, cand.EnvironmentID); err != nil {
		slog.Warn("pitr: candidate not removed", "tile", cand.ID, "err", err)
		return
	} else if _, err := o.promote.Remove(ctx, e, []Tile{cand}, log); err != nil {
		_, _ = fmt.Fprintf(log, "warning: %s not removed: %v\n", cand.Slug, err)
		return
	}
	_ = o.store.Settings.Delete(ctx, candKey+cand.ID)
	if !hard {
		return
	}
	for _, v := range vs {
		if err := o.volumes.Delete(ctx, v, nil); err != nil {
			_, _ = fmt.Fprintf(log, "warning: volume %s stays: %v\n", v.Slug, err)
		}
	}
}

// sweepCandidates drops settings rows of candidates whose tile is gone, and
// temporary candidates a crashed job left behind.
func (o *Orchestrator) sweepCandidates(ctx context.Context) {
	rows, err := o.store.Settings.ListPrefix(ctx, candKey)
	if err != nil {
		return
	}
	for k, v := range rows {
		id := strings.TrimPrefix(k, candKey)
		t, err := o.tiles.Get(ctx, id)
		if errors.Is(err, errs.ErrNotFound) {
			_ = o.store.Settings.Delete(ctx, k)
			continue
		}
		var c candidate
		if err != nil || json.Unmarshal([]byte(v), &c) != nil || !c.Temp || time.Since(c.Created) < pitrTempMaxAge {
			continue
		}
		if jctx, err := o.withTileNode(ctx, t.ID); err == nil {
			o.dropCandidate(jctx, t, true, io.Discard)
		}
	}
}

// ---- branch --at ----

// restoreTemp restores the source instance of a branch to atStr into a
// scratch candidate and returns it; the caller drops it with dropCandidate.
func (o *Orchestrator) restoreTemp(ctx context.Context, r *jobs.Run, srcInst Tile, atStr string) (Tile, error) {
	src, err := o.pitrSourceOf(ctx, srcInst.ID)
	if err != nil {
		return Tile{}, err
	}
	release := o.backup.Hold(src.root)
	defer release()
	plan, at, latest, err := o.pitrPlan(ctx, src, atStr, false)
	if err != nil {
		return Tile{}, err
	}
	cand, err := o.newCandidate(ctx, src, time.Now(), normAt(at, latest), true)
	if err != nil {
		return Tile{}, err
	}
	fail := func(err error) (Tile, error) {
		o.dropCandidate(ctx, cand, true, r.Log)
		return Tile{}, err
	}
	if err := o.fillCandidate(ctx, r, src, cand, plan, at, latest); err != nil {
		return fail(err)
	}
	release()
	if err := o.startAndAwait(ctx, r, src, cand, plan, at, latest, false); err != nil {
		return fail(err)
	}
	return cand, nil
}

// seedFromSliceAt is seedFromSlice as of at: the source instance is restored
// to a scratch copy, the slice's database is dumped out of it into the load,
// and the copy goes, volume and all.
func (o *Orchestrator) seedFromSliceAt(ctx context.Context, r *jobs.Run, s Tile, p seedJob) error {
	src, err := o.tiles.Get(ctx, p.Source.SliceID)
	if err != nil {
		return err
	}
	srcInst, err := o.postgresOf(ctx, src)
	if err != nil {
		return err
	}
	rctx, err := o.withTileNode(ctx, srcInst.ID) // the copy runs where the source does
	if err != nil {
		return err
	}
	cand, err := o.restoreTemp(rctx, r, srcInst, p.Source.At)
	if err != nil {
		return err
	}
	defer o.dropCandidate(rctx, cand, true, r.Log)
	_, _ = fmt.Fprintf(r.Log, "dumping %s as of %s\n", src.Slug, p.Source.At)
	out, wait, err := o.engines.DumpSliceOn(rctx, src, cand)
	if err != nil {
		return err
	}
	err = o.engines.Seed(ctx, s, p.Format, out, r.Log)
	return errors.Join(err, wait())
}

// ---- promote ----

// PromoteRestore queues the promotion of restored candidate candID over its
// source: the source's data volume is swapped for the candidate's, so apps
// keep their address and credentials. The caller needs tile.write on the
// source's stack.
func (o *Orchestrator) PromoteRestore(ctx context.Context, by *Principal, candID string) (Job, error) {
	c, ok := o.candidateOf(ctx, candID)
	if !ok || c.Temp {
		return Job{}, errs.Conflictf("that tile is not a restored copy of an instance")
	}
	cand, err := o.tiles.Get(ctx, candID)
	if err != nil {
		return Job{}, err
	}
	src, err := o.tiles.Get(ctx, c.Source)
	if err != nil {
		return Job{}, errs.Conflictf("the instance this was restored from is gone")
	}
	st, err := o.stacks.Get(ctx, src.StackID)
	if err != nil {
		return Job{}, err
	}
	if err := mayWrite(by, st); err != nil {
		return Job{}, err
	}
	if err := o.gateTile(ctx, src.ID, true); err != nil {
		return Job{}, err
	}
	if err := o.promoteAllowed(ctx, src); err != nil {
		return Job{}, err
	}
	sv, err := o.dataVolume(ctx, src)
	if err != nil {
		return Job{}, err
	}
	lock, err := o.instanceLocks(ctx, sv)
	if err != nil {
		return Job{}, err
	}
	lock = append(lock, "volume:"+sv.ID, cand.ID)
	j, err := o.enqueue(ctx, kindPromoteRestore, promoteRestoreJob{SourceID: src.ID, CandidateID: cand.ID}, lock...)
	if err != nil {
		return Job{}, err
	}
	o.auditBy(ctx, by, securitylog.InstancePromote, src.ID, fmt.Sprintf("%s over %s, at=%s", cand.Slug, src.Slug, c.At))
	return j, nil
}

// instanceLocks is the lock set of work that swaps what an instance runs:
// the instance, its slice tiles and the tiles bound to them. A deploy locks
// its own tile only, so none of them runs Provision or Bind meanwhile.
func (o *Orchestrator) instanceLocks(ctx context.Context, v store.Volume) ([]string, error) {
	if v.InstanceID == nil {
		return nil, nil
	}
	it, err := o.instanceTile(ctx, *v.InstanceID)
	if err != nil {
		return nil, err
	}
	lock := []string{it.ID}
	ps, err := o.operated.ByInstance(ctx, *v.InstanceID)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		lock = append(lock, p.TileID)
	}
	cs, err := o.consumers(ctx, v)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		lock = append(lock, c.ID)
	}
	return lock, nil
}

type promoteRestoreJob struct {
	SourceID    string `json:"source_id"`
	CandidateID string `json:"candidate_id"`
}

// promoteSteps are the actions of a promote, in the order it runs them; the
// fakes of the order test stand in for a real instance. Until swap commits
// nothing has changed but the source being stopped, which undo reverts: the
// archive is only listed (check). After it the source's row names the new data
// and a re-run (swapped) resumes at stopCand: swapping again would hand the old
// data back. The source starts before its history goes up (from its own
// pg_wal), so a failed upload leaves it running, and the candidate goes last,
// after the archive reset, so every later step can be re-run.
type promoteSteps struct {
	swapped  bool         // the swap already committed in an earlier run
	ready    func() error // the candidate finished replaying
	check    func() error // the archive does not hold the copy's timeline yet (lists only)
	stopSrc  func() error
	swap     func() error // the point of no return: the volume rows trade data, in one transaction
	undo     func()       // a failure before the swap: the source runs again
	stopCand func() error
	start    func() error // the source redeploys on the new data
	history  func() error // the new timeline's history goes to the archive, from the source
	replay   func() error // Provision and Bind for every row
	settle   func() error // the archive state is reset and a base queued
	drop     func() error // the candidate's tile goes, not its volume
}

type promoteStep struct {
	name string
	do   func() error
}

func (s promoteSteps) run(log io.Writer) error {
	pre := []promoteStep{{"check the restored copy", s.ready}, {"check the archive's timelines", s.check},
		{"stop the source", s.stopSrc}, {"swap the data volume", s.swap}}
	post := []promoteStep{{"stop the restored copy", s.stopCand}, {"start the source", s.start},
		{"ship its timeline history", s.history}, {"re-apply slices and bindings", s.replay},
		{"reset the archive state", s.settle}, {"remove the restored tile", s.drop}}
	if s.swapped {
		_, _ = fmt.Fprintf(log, "the swap already happened; resuming after it\n")
		pre = nil
	}
	n := len(pre)
	for i, st := range append(pre, post...) {
		if st.do == nil {
			continue
		}
		_, _ = fmt.Fprintf(log, "%s\n", st.name)
		if err := st.do(); err != nil {
			if i < n && s.undo != nil {
				s.undo()
			}
			return fmt.Errorf("%s: %w", st.name, err)
		}
	}
	return nil
}

// promoteAllowed refuses a promote over a fenced primary (it would stop it
// and start it again) or one with a standby (the standby would follow data
// that no longer exists). Asked when queued and again when it runs.
func (o *Orchestrator) promoteAllowed(ctx context.Context, src Tile) error {
	if err := o.mayStartTile(ctx, src); err != nil {
		return err
	}
	m, err := o.operated.GetByTile(ctx, src.ID)
	if err != nil {
		return err
	}
	return o.noStandbys(ctx, m.ID)
}

func (o *Orchestrator) runPromoteRestore(ctx context.Context, r *jobs.Run, p promoteRestoreJob) error {
	src, err := o.tiles.Get(ctx, p.SourceID)
	if err != nil {
		return err
	}
	cand, err := o.tiles.Get(ctx, p.CandidateID)
	if err != nil {
		return err
	}
	if ctx, err = o.withTileNode(ctx, src.ID); err != nil {
		return err
	}
	o.ensureMesh(ctx)
	sm, err := o.operated.GetByTile(ctx, src.ID)
	if err != nil {
		return err
	}
	sv, err := o.dataVolume(ctx, src)
	if err != nil {
		return err
	}
	cv, err := o.dataVolume(ctx, cand)
	if err != nil {
		return err
	}
	if cv.OrphanedAt != nil { // a resume: the source must still run this copy's data
		if c, _ := o.candidateOf(ctx, cand.ID); c.Vol != sv.Name {
			return errs.Conflictf("another copy was promoted over %s since; restore again from its current state", src.Slug)
		}
	} else if err := o.promoteAllowed(ctx, src); err != nil { // before stopSrc: nothing destructive yet
		return err
	}
	candWal, err := o.engines.WalDir(ctx, cand)
	if err != nil {
		return err
	}
	srcWal, err := o.engines.WalDir(ctx, src)
	if err != nil {
		return err
	}
	running := func(t Tile) bool {
		cs, err := o.tiles.Replicas(ctx, t)
		return err == nil && slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" })
	}
	stopped := false
	return promoteSteps{
		swapped: cv.OrphanedAt != nil, // only the swap orphans a copy's data volume while its tile lives
		ready: func() error {
			if rec, err := o.engines.Recovering(ctx, cand); err != nil || rec {
				return errs.Conflictf("%s is not done replaying (or is not running); wait for its restore job, then promote", cand.Slug)
			}
			return nil
		},
		check: func() error {
			if sm.PITRDays <= 0 {
				return nil
			}
			s, err := o.pitrSourceOf(ctx, src.ID)
			if err != nil {
				return err
			}
			if c, ok := o.candidateOf(ctx, cand.ID); ok && c.Shipped {
				return nil
			}
			return o.backup.CheckHistory(ctx, fbackup.Ship{Engine: cand, Dest: s.dest, Root: s.root, Dir: candWal})
		},
		history: func() error {
			if sm.PITRDays <= 0 {
				return nil
			}
			s, err := o.pitrSourceOf(ctx, src.ID)
			if err != nil {
				return err
			}
			if c, ok := o.candidateOf(ctx, cand.ID); ok && c.Shipped {
				return nil // an earlier run shipped it; the archive holds its own history now
			}
			if err := o.backup.ShipHistory(ctx, fbackup.Ship{Engine: src, Dest: s.dest, Root: s.root, Dir: srcWal}, r.Log); err != nil {
				return err
			}
			c, _ := o.candidateOf(ctx, cand.ID)
			c.Shipped = true
			return o.setCandidate(ctx, cand.ID, c)
		},
		stopSrc: func() error {
			if !running(src) {
				return nil
			}
			stopped = true
			return o.container.Stop(ctx, src, r.Log)
		},
		undo: func() {
			if stopped {
				if err := o.mayStartTile(ctx, src); err != nil { // a fence set meanwhile still holds
					_, _ = fmt.Fprintf(r.Log, "warning: %s stays down: %v\n", src.Slug, err)
					return
				}
				if err := o.container.Start(context.WithoutCancel(ctx), src, r.Log); err != nil {
					_, _ = fmt.Fprintf(r.Log, "warning: %s did not restart: %v\n", src.Slug, err)
				}
			}
		},
		stopCand: func() error {
			if !running(cand) {
				return nil
			}
			return o.container.Stop(ctx, cand, r.Log)
		},
		swap: func() error {
			if err := r.Swap(); err != nil {
				return err
			}
			c, _ := o.candidateOf(ctx, cand.ID)
			c.Vol = cv.Name
			if err := o.setCandidate(ctx, cand.ID, c); err != nil {
				return err
			}
			return o.swapData(ctx, sv, cv)
		},
		start: func() error {
			if err := o.deploy.Redeploy(ctx, src.ID, r.Log, nil); err != nil {
				o.downAfterFailedDeploy(ctx, src.ID, err)
				return err
			}
			return o.afterDeploy(ctx, []string{src.ID}, false)
		},
		replay: func() error { return o.engines.Replay(ctx, src, r.Log) },
		drop: func() error {
			o.dropCandidate(ctx, cand, false, r.Log) // the volume is the old data: orphaned, undo until retention
			return nil
		},
		settle: func() error {
			if err := o.pitrReset(ctx, sm); err != nil {
				return err
			}
			if sm.PITRDays > 0 {
				if _, err := o.BackupNow(ctx, sv.ID, "", "base", ""); err != nil {
					return err
				}
			}
			return nil
		},
	}.run(r.Log)
}

// pitrReset starts a new archive epoch from now, set before the base is
// queued so that base counts; the guard runs again against the same prefix,
// as its own. Under the shipper's lock: a tick in flight cannot save the old
// state over it afterwards. With PITR off the state just goes.
func (o *Orchestrator) pitrReset(ctx context.Context, m store.OperatedInstance) error {
	ctx = context.WithoutCancel(ctx) // a cancelled request must not lose the reset
	mu := o.pitrLock(m.ID)
	mu.Lock()
	defer mu.Unlock()
	var err error
	if m.PITRDays > 0 {
		now := time.Now().UTC()
		err = o.putPITRState(ctx, m.ID, pitrState{Since: now, ArchSince: now})
	} else {
		err = o.store.Settings.Delete(ctx, pitrKey(m.ID))
	}
	o.clearPITRProblems(ctx, m.ID)
	return err
}

// swapData makes the source's data volume row name the candidate's Docker
// volume, and the candidate's row (orphaned) the source's old one: the old
// data stays restorable until orphan retention. One transaction: a crash
// leaves the rows as they were or fully swapped, never both naming one volume.
func (o *Orchestrator) swapData(ctx context.Context, sv, cv store.Volume) error {
	sv.Name, cv.Name = cv.Name, sv.Name
	now := time.Now().UTC()
	cv.OrphanedAt = &now
	return o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.Volumes.Update(ctx, sv); err != nil {
			return err
		}
		return tx.Volumes.Update(ctx, cv)
	})
}
