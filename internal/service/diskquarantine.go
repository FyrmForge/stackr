package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Quarantine: a node whose disk is missing or erroring. A missing disk stops
// what runs against it (this file's job); an erroring one only blocks new
// work on it (failedDiskGate). Check disk is the way out for both.

const kindDiskQuarantine jobs.Kind = "disk-quarantine"

// diskQuarJob is the disk-quarantine payload.
type diskQuarJob struct {
	DiskID string `json:"disk_id"`
	NodeID string `json:"node_id"`
}

// quarRec is what a quarantine stopped, kept in a settings row from before the
// first stop to Check disk's resume. Done is set once every stop is made: a row
// without it is a job that has to run again, and keeps what it already stopped.
// Since is the missing problem's start, so a row left by an earlier outage is
// not taken for this one.
type quarRec struct {
	Tiles   []string  `json:"tiles"`
	Members []string  `json:"members"`
	Done    bool      `json:"done"`
	Since   time.Time `json:"since"`
	// after Check disk: the redeploy job queued for each item ("t:<tile>"), and
	// how many times each was tried
	Jobs  map[string]string `json:"jobs,omitempty"`
	Tries map[string]int    `json:"tries,omitempty"`
}

// quarRetry is how long a failed quarantine waits before the watch queues it again.
const quarRetry = 5 * time.Minute

// quarForget drops every row a disk left behind: the quarantine record, the
// erroring latch and the problem. Called when the disk row itself goes.
func (o *Orchestrator) quarForget(ctx context.Context, diskID string) {
	for _, k := range []string{quarKey(diskID), erroringKey(diskID)} {
		if err := o.store.Settings.Delete(ctx, k); err != nil {
			slog.Warn("disk: row not deleted", "key", k, "err", err)
		}
	}
	o.setLatchInMemory(diskID, false)
	o.clearProblem(ctx, quarProblemKey(diskID))
}

func quarKey(diskID string) string { return "diskquarantine." + diskID }

func (o *Orchestrator) quarLoad(ctx context.Context, diskID string) (quarRec, bool, error) {
	s, ok, err := o.store.Settings.Get(ctx, quarKey(diskID))
	if err != nil || !ok {
		return quarRec{}, false, err
	}
	var r quarRec
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return quarRec{}, false, nil // unreadable is as good as none: the job runs again
	}
	return r, true, nil
}

func (o *Orchestrator) quarSave(ctx context.Context, diskID string, r quarRec) error {
	b, _ := json.Marshal(r)
	return o.store.Settings.Set(ctx, quarKey(diskID), string(b))
}

// quarantineEnqueue queues the node-level disk-quarantine job for a missing
// disk unless one completed since the latch (the row says so, so a panel
// restart between the latch and the stop queues it again) or one is queued or
// running. The enqueue is exclusive under the node and disk lock.
func (o *Orchestrator) quarantineEnqueue(ctx context.Context, n store.Node, d store.NodeDisk) {
	rec, ok, err := o.quarLoad(ctx, d.ID)
	if err != nil || (ok && rec.Done && rec.Since.Equal(o.diskMissingSince(ctx, n.ID))) {
		return
	}
	// a job that failed is tried again, but not on every read: each one takes the node lock
	if js, err := o.jobRows.History(ctx, []string{"disk:" + d.ID}, 10); err == nil {
		if i := slices.IndexFunc(js, func(j store.Job) bool { return j.Kind == string(kindDiskQuarantine) }); i >= 0 &&
			js[i].State == job.Failed && time.Since(js[i].CreatedAt) < quarRetry {
			return
		}
	}
	lock, err := o.nodeLock(ctx, n)
	if err != nil {
		return
	}
	lock = append(lock, "disk:"+d.ID)
	_, err = o.enqueueExclusive(ctx, kindDiskQuarantine, diskQuarJob{DiskID: d.ID, NodeID: n.ID},
		[]string{"disk:" + d.ID}, errs.Conflictf("A quarantine of this disk is already queued."), lock...)
	if _, busy := errs.IsConflict(err); err != nil && !busy {
		slog.Warn("disk quarantine: not queued", "node", n.ID, "err", err)
	}
}

// runDiskQuarantine stops what runs against a missing disk: the ordinary tiles
// with a volume on it and the standbys whose data folder is on it. The affected
// set is built the way Forget builds it. A tile that is an operated primary with a standby, and
// a primary that failed over onto the disk are left to the failover flow. The
// job only records and stops: it never touches State, Fenced, role or routing
// of a member (only the sync pass moves its sync_in flag).
func (o *Orchestrator) runDiskQuarantine(ctx context.Context, r *jobs.Run, p diskQuarJob) error {
	d, err := o.store.NodeDisks.Get(ctx, p.DiskID)
	if err != nil {
		return err
	}
	if d.State != disk.StateMissing {
		_, _ = fmt.Fprintln(r.Log, "the disk is not missing: nothing to stop")
		return nil
	}
	if _, err := o.diskNodeUp(ctx, d.NodeID); err != nil {
		return err
	}
	s, err := o.forgetSet(ctx, d)
	if err != nil {
		return err
	}
	rec, _, err := o.quarLoad(ctx, d.ID)
	if err != nil {
		return err
	}
	rec.Since, rec.Done = o.diskMissingSince(ctx, d.NodeID), false

	var tiles []store.Tile
	for _, e := range s.envs {
		for _, t := range s.tiles[e.ID] {
			if inst, err := o.operated.GetByTile(ctx, t.ID); err == nil {
				// a primary with a standby is left to failover; a standalone one has none
				if _, has, err := o.members.LiveReplica(ctx, inst.ID); err != nil {
					return err
				} else if has {
					_, _ = fmt.Fprintf(r.Log, "%s is an operated primary with a standby: left to failover\n", t.Slug)
					continue
				}
			}
			up := slices.Contains(rec.Tiles, t.ID)
			if !up {
				// a list that fails is an error, not "nothing runs": the job runs again
				cs, err := o.tiles.Replicas(ctx, t)
				if err != nil {
					return err
				}
				up = slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" || c.State == "restarting" })
			}
			if up {
				tiles = append(tiles, t)
				if !slices.Contains(rec.Tiles, t.ID) {
					rec.Tiles = append(rec.Tiles, t.ID)
				}
			}
		}
	}
	var members []InstanceMember
	for _, m := range s.members {
		if m.Role == opmember.Primary {
			_, _ = fmt.Fprintf(r.Log, "member %s is a primary: left to failover\n", m.ID)
			continue
		}
		up := slices.Contains(rec.Members, m.ID)
		if !up {
			var err error
			if up, err = o.memberRunning(ctx, m); err != nil {
				return err
			}
		}
		if up {
			members = append(members, m)
			if !slices.Contains(rec.Members, m.ID) {
				rec.Members = append(rec.Members, m.ID)
			}
		}
	}
	// recorded before the first stop: a crash after one is finished by the rerun
	if err := o.quarSave(ctx, d.ID, rec); err != nil {
		return err
	}
	var failed []error
	// a standby leaves the sync list before it stops, or the primary's commits
	// hang: one sync pass per instance, which leaves out a standby on a missing disk
	stoppable, dropped := map[string]bool{}, map[string]error{}
	for _, m := range members {
		if _, done := dropped[m.InstanceID]; !done {
			dropped[m.InstanceID] = o.dropFromSync(ctx, m.InstanceID)
		}
		if err := dropped[m.InstanceID]; err != nil {
			failed = append(failed, fmt.Errorf("member %s: %w", m.ID, err))
			continue
		}
		stoppable[m.ID] = true
	}
	for _, t := range tiles {
		if err := o.container.Stop(ctx, t, io.Discard); err != nil {
			failed = append(failed, fmt.Errorf("stop %s: %w", t.Slug, err))
			continue
		}
		o.clearProblem(ctx, downAlertKey(t.ID))
		_, _ = fmt.Fprintf(r.Log, "stopped %s\n", t.Slug)
	}
	for _, m := range members {
		if !stoppable[m.ID] {
			continue
		}
		if err := o.stopMember(ctx, m); err != nil {
			failed = append(failed, fmt.Errorf("stop member %s: %w", m.ID, err))
			continue
		}
		_, _ = fmt.Fprintf(r.Log, "stopped member %s\n", m.ID)
	}
	if len(failed) > 0 {
		return errors.Join(failed...)
	}
	rec.Done = true
	return o.quarSave(ctx, d.ID, rec)
}

// memberRunning: member m has a running container on its node.
func (o *Orchestrator) memberRunning(ctx context.Context, m InstanceMember) (bool, error) {
	cs, err := o.docker.List(docker.WithNode(ctx, m.NodeID), map[string]string{LabelMember: m.ID})
	return slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" || c.State == "restarting" }), err
}

// dropFromSync runs the instance's sync pass (under its topology lock), which
// leaves out a standby whose disk is missing, so the primary stops waiting for
// it and the pass clears the member's sync_in flag. A pass that cannot run (busy,
// failing over) or fails is an error: the standby stays up and the job runs
// again. Only a primary seen exited or absent waits for nobody, so the stop goes on.
func (o *Orchestrator) dropFromSync(ctx context.Context, instanceID string) error {
	inst, err := o.operated.Get(ctx, instanceID)
	if err != nil {
		return err
	}
	t, err := o.tiles.Get(ctx, inst.TileID)
	if err != nil {
		return err
	}
	err = o.syncInstance(ctx, t, inst)
	if err == nil {
		return nil
	}
	// the stop goes on only when the primary is positively not running: a failed
	// look, a restarting container or a running one keeps the standby up
	cs, rerr := o.tiles.Replicas(ctx, t)
	if rerr != nil {
		return errors.Join(err, rerr)
	}
	if slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State != "exited" && c.State != "dead" }) {
		return err
	}
	return nil
}

// resumeQuarantined is Check disk's resume of what the quarantine of d
// stopped: the recorded tiles are deployed (a job each, remembered) and the
// standbys started, which keeps their rows. The record stays until quarSettle
// has seen every item run, so it is not lost to a redeploy that fails later.
// With no record it is the plain redeploy of whatever is not running. Pressing
// Check disk again gives every item its tries back.
func (o *Orchestrator) resumeQuarantined(ctx context.Context, r *jobs.Run, d store.NodeDisk) error {
	rec, ok, err := o.quarLoad(ctx, d.ID)
	if err != nil {
		return err
	}
	var only map[string]bool
	if ok {
		rec.Done = false // the next outage gets its own quarantine
		only = map[string]bool{}
		for _, id := range rec.Tiles {
			only[id] = true
		}
	}
	ids, derr := o.deployDiskTiles(ctx, r, d, only)
	failed := []error{derr}
	if ok {
		rec.Jobs, rec.Tries = map[string]string{}, map[string]int{}
		for id, j := range ids {
			rec.Jobs["t:"+id], rec.Tries["t:"+id] = j, 1
		}
		if err := o.quarSave(ctx, d.ID, rec); err != nil {
			failed = append(failed, err)
		}
		failed = append(failed, o.quarSettle(ctx, d))
	}
	return errors.Join(failed...)
}

// DiskErroring is whether the node's ready disk is latched erroring (the Nodes
// list shows it as Disk failed, like a missing one).
func (o *Orchestrator) DiskErroring(ctx context.Context, nodeID string) (bool, error) {
	return o.diskErroring(ctx, nodeID)
}

// failedDisk is the node's disk when it is missing or latched erroring. An
// error is an answer the callers refuse on: a disk that cannot be read is not
// taken for a fine one.
func (o *Orchestrator) failedDisk(ctx context.Context, nodeID string) (store.NodeDisk, bool, error) {
	d, has, err := o.nodeDisk(ctx, nodeID)
	if err != nil || !has {
		return d, false, err
	}
	if d.State == disk.StateMissing {
		return d, true, nil
	}
	if d.State != disk.StateReady {
		return d, false, nil
	}
	e, err := o.diskErroring(ctx, nodeID)
	return d, e, err
}

// failedDiskGate refuses a deploy of env e when its node's disk has failed and
// the env has a volume on it or would create one. A stateless env passes.
func (o *Orchestrator) failedDiskGate(ctx context.Context, e store.Environment) error {
	d, failed, err := o.failedDisk(ctx, e.NodeID)
	if err != nil {
		return err
	}
	if !failed {
		return nil
	}
	fs, err := o.diskLeaf.Folders(ctx, d)
	if err != nil {
		return err
	}
	on := map[string]bool{}
	for _, f := range fs {
		on[f.Name] = true
	}
	vs, err := o.pmVolumes(ctx, e.ID)
	if err != nil {
		return err
	}
	uses := slices.ContainsFunc(vs, func(v store.Volume) bool { return on[v.Name] })
	nctx := docker.WithNode(ctx, e.NodeID)
	for _, v := range vs { // declared, no folder and no Docker object yet: a deploy makes it on the disk
		if uses {
			break
		}
		found, _, err := o.docker.VolumeDevice(nctx, v.Name)
		if err != nil {
			return err
		}
		if !found {
			uses = true
		}
	}
	if !uses {
		ts, err := o.tiles.List(ctx, e.ID)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, v := range vs {
			have[v.Slug] = true
		}
		for _, t := range ts { // a volume line with no volume yet would be created on the disk
			for _, l := range tile.Lines(t.Volumes) {
				if sl, _, ok := MountedVolume(l); ok && !have[sl] {
					uses = true
				}
			}
		}
	}
	if !uses {
		return nil
	}
	word := "failing"
	if d.State == disk.StateMissing {
		word = "missing"
	}
	return errs.Refusedf("The disk on this node is %s and this env keeps data on it. Check the disk first.", word)
}

// erroringDiskGate refuses new data on a node whose disk is latched erroring:
// a move of volumes or an env onto it. Missing is the volume leaf's to refuse.
func (o *Orchestrator) erroringDiskGate(ctx context.Context, nodeID string) error {
	if nodeID == "" {
		return nil
	}
	d, has, err := o.nodeDisk(ctx, nodeID)
	if err != nil || !has || d.State != disk.StateReady {
		return err
	}
	if e, err := o.diskErroring(ctx, nodeID); err != nil || !e {
		return err
	}
	return errs.Refusedf("The disk on %s is failing. Check the disk first.", o.nodeName(ctx, nodeID))
}

// volumeOnFailedDisk: folder name is on the failed (missing or latched
// erroring) disk of node.
func (o *Orchestrator) volumeOnFailedDisk(ctx context.Context, node, name string) (bool, error) {
	d, failed, err := o.failedDisk(ctx, node)
	if err != nil || !failed {
		return false, err
	}
	fs, err := o.diskLeaf.Folders(ctx, d)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(fs, func(f store.DiskFolder) bool { return f.Name == name }), nil
}

// memberDiskHeld: standby m's data folder is on a failed disk. The tick's
// startMembers must not bring it back before Check disk has. A disk that
// cannot be read holds it too.
func (o *Orchestrator) memberDiskHeld(ctx context.Context, m InstanceMember) bool {
	if m.Role != opmember.Replica || m.Volume == "" {
		return false
	}
	bad, err := o.volumeOnFailedDisk(ctx, m.NodeID, m.Volume)
	return bad || err != nil
}

// primaryDiskDown: the primary of an instance runs on node, and its data is
// on a failed disk there. After a failover that is the member's own volume on
// its node; before, the env's volumes on the env's node.
func (o *Orchestrator) primaryDiskDown(ctx context.Context, t store.Tile, m InstanceMember, node string) bool {
	if m.Volume != "" {
		bad, err := o.volumeOnFailedDisk(ctx, node, m.Volume)
		return bad || err != nil
	}
	e, err := o.store.Environments.Get(ctx, t.EnvironmentID)
	if err != nil || e.NodeID == "" || e.NodeID != node {
		return false
	}
	d, failed, err := o.failedDisk(ctx, node)
	if err != nil {
		return true
	}
	if !failed {
		return false
	}
	fs, err := o.diskLeaf.Folders(ctx, d)
	if err != nil {
		return true
	}
	on := map[string]bool{}
	for _, f := range fs {
		on[f.Name] = true
	}
	ts, err := o.diskTiles(ctx, e, on)
	return err != nil || slices.ContainsFunc(ts, func(x Tile) bool { return x.ID == t.ID })
}

// memberDiskDown: standby m's data folder is on a missing disk; the sync pass
// leaves it out (it is stopped), where an erroring disk's standby still runs.
func (o *Orchestrator) memberDiskDown(ctx context.Context, m InstanceMember) bool {
	if m.Role != opmember.Replica || m.Volume == "" {
		return false
	}
	d, has, err := o.nodeDisk(ctx, m.NodeID)
	if err != nil || !has || d.State != disk.StateMissing {
		return false
	}
	fs, err := o.diskLeaf.Folders(ctx, d)
	return err == nil && slices.ContainsFunc(fs, func(f store.DiskFolder) bool { return f.Name == m.Volume })
}

// runGateTile is the storage gate for anything that starts the tile's
// containers (start, restart, a manual run, and a job that was queued before
// the disk failed): it refuses a tile whose env has data on a failed disk.
func (o *Orchestrator) runGateTile(ctx context.Context, tileID string) error {
	t, err := o.store.Tiles.Get(ctx, tileID)
	if errors.Is(err, errs.ErrNotFound) {
		return nil // gone: the job's own lookup says so
	}
	if err != nil {
		return err
	}
	// an operated primary that failed over runs on its member's node, on the
	// member's own volume: that is the disk to ask, not the env's
	inst, err := o.operated.GetByTile(ctx, t.ID)
	switch {
	case errors.Is(err, errs.ErrNotFound): // not operated
	case err != nil:
		return err
	default:
		ms, err := o.members.List(ctx, inst.ID)
		if err != nil {
			return err
		}
		if p, ok := opmember.PrimaryOf(ms); ok && p.Volume != "" {
			node, err := o.primaryNode(ctx, t)
			if err != nil {
				return err
			}
			bad, err := o.volumeOnFailedDisk(ctx, node, p.Volume)
			if err != nil {
				return err
			}
			if bad {
				return errs.Refusedf("The disk on %s has failed and this database keeps data on it. Check the disk first.", o.nodeName(ctx, node))
			}
			return nil
		}
	}
	return o.runGateEnv(ctx, t.EnvironmentID)
}

// gatedSwap is a job's swap marker with the storage gate in front of it: the
// first container change of a deploy, promote or env sync comes after a prep
// that can take minutes, in which the disk may have failed.
func gatedSwap(swap func() error, gate func() error) func() error {
	if swap == nil {
		return nil
	}
	return func() error {
		if err := gate(); err != nil {
			return err
		}
		return swap()
	}
}

func (o *Orchestrator) runGateEnv(ctx context.Context, envID string) error {
	e, err := o.store.Environments.Get(ctx, envID)
	if errors.Is(err, errs.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if e.NodeID == "" {
		return nil
	}
	return o.failedDiskGate(ctx, e)
}

// ---- the erroring latch, the check guard, the stopped set's settling

func erroringKey(diskID string) string { return "diskerroring." + diskID }

// diskErroring: the node's disk is latched erroring. The latch is a settings
// row (and a memory copy, kept when the write failed), so it survives a panel
// restart; only Check disk clears it. A read that fails is an error, which the
// gates refuse on.
func (o *Orchestrator) diskErroring(ctx context.Context, nodeID string) (bool, error) {
	d, has, err := o.nodeDisk(ctx, nodeID)
	if err != nil || !has {
		return false, err
	}
	if o.latchInMemory(d.ID) {
		return true, nil
	}
	_, ok, err := o.store.Settings.Get(ctx, erroringKey(d.ID))
	return ok, err
}

func (o *Orchestrator) latchInMemory(diskID string) bool {
	w := o.alertWatch()
	w.diskMu.Lock()
	defer w.diskMu.Unlock()
	return w.diskBad["err:"+diskID] == 1
}

func (o *Orchestrator) setLatchInMemory(diskID string, v bool) {
	w := o.alertWatch()
	w.diskMu.Lock()
	defer w.diskMu.Unlock()
	if v {
		w.diskBad["err:"+diskID] = 1
	} else {
		delete(w.diskBad, "err:"+diskID)
	}
}

// latchErroring holds the disk erroring now, in memory, and writes it down. A
// write that fails is tried again by flushLatch on the next read.
func (o *Orchestrator) latchErroring(ctx context.Context, d store.NodeDisk) {
	o.setLatchInMemory(d.ID, true)
	o.flushLatch(ctx, d)
}

func (o *Orchestrator) flushLatch(ctx context.Context, d store.NodeDisk) {
	if !o.latchInMemory(d.ID) {
		return
	}
	if _, ok, err := o.store.Settings.Get(ctx, erroringKey(d.ID)); err == nil && ok {
		return
	}
	if err := o.store.Settings.Set(ctx, erroringKey(d.ID), time.Now().UTC().Format(time.RFC3339)); err != nil {
		slog.Warn("disk: erroring latch not written, kept in memory", "disk", d.ID, "err", err)
	}
}

// clearErroring ends the latch, the written one first: a failed delete leaves
// it held.
func (o *Orchestrator) clearErroring(ctx context.Context, d store.NodeDisk) error {
	if err := o.store.Settings.Delete(ctx, erroringKey(d.ID)); err != nil {
		return err
	}
	o.setLatchInMemory(d.ID, false)
	return nil
}

// diskGuard serializes Check disk's verify with the watch's latching of the
// same node's disk, and remembers when the last check finished: reads taken
// before that say nothing about the disk now.
type diskGuard struct {
	mu    sync.Mutex
	epoch atomic.Int64 // UnixNano, controller clock: when the last Check disk finished
	hw    atomic.Int64 // UnixNano, worker clock: the newest read the controller had then
}

func nanoTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// superseded: a read that a Check disk has overtaken. polled is when the
// controller started the poll that brought it (the controller's clock; zero
// for the controller's own worker, whose reads are on the same clock). A poll
// that began before the check ended brought nothing newer than it; a read no
// newer than the newest one the controller held at the check (compared on the
// worker's own clock, so skew does not matter) is the same old read again.
func (g *diskGuard) superseded(polled, readAt time.Time) bool {
	e := nanoTime(g.epoch.Load())
	if e.IsZero() {
		return false
	}
	if !polled.IsZero() && !polled.After(e) {
		return true
	}
	if polled.IsZero() && !readAt.IsZero() && !readAt.After(e) {
		return true
	}
	hw := nanoTime(g.hw.Load())
	return !hw.IsZero() && !readAt.IsZero() && !readAt.After(hw)
}

// diskReadHW is the newest read of the disk the controller holds now.
func (o *Orchestrator) diskReadHW(ctx context.Context, d store.NodeDisk) time.Time {
	n, err := o.store.Nodes.Get(ctx, d.NodeID)
	if err != nil {
		return time.Time{}
	}
	f := o.diskFacts(ctx, n)
	if f == nil {
		return time.Time{}
	}
	if i := slices.IndexFunc(f.Disks, func(x hostdisk.Fact) bool { return x.Path == d.MountPath }); i >= 0 {
		return f.Disks[i].ReadAt
	}
	return time.Time{}
}

func (o *Orchestrator) diskGuard(nodeID string) *diskGuard {
	g, _ := o.alertWatch().diskGuards.LoadOrStore(nodeID, &diskGuard{})
	return g.(*diskGuard)
}

// quarTries is how many times a pending item is redeployed or started again
// before it is left pending with its problem open.
const quarTries = 3

func quarProblemKey(diskID string) string { return "alert.diskquarantine." + diskID }

// startKinds are the jobs that bring a tile's containers up.
var startKinds = []string{string(kindDeploy), string(kindPromote), string(kindEnvSync), string(kindStart), string(kindRestart)}

// newestStart is the newest job of the tile that starts its containers.
func (o *Orchestrator) newestStart(ctx context.Context, t store.Tile) (store.Job, bool, error) {
	js, err := o.jobRows.History(ctx, []string{t.ID}, 30)
	if err != nil {
		return store.Job{}, false, err
	}
	i := slices.IndexFunc(js, func(j store.Job) bool { return slices.Contains(startKinds, j.Kind) })
	if i < 0 {
		return store.Job{}, false, nil
	}
	return js[i], true, nil
}

// quarSettle works the stopped set of a resumed quarantine down. A tile is
// done when its newest start job finished and it runs, when it is gone, paused
// or was stopped by the person since; one whose job is still live waits (a
// replica that is up in the middle of a health check is not done); one whose
// job failed, or is gone after a crash, is deployed again, up to quarTries.
// A standby is done when it runs, and is started again the same way. What is
// left past its tries stays pending with a problem open. The record goes when
// nothing is pending, and is rewritten otherwise: a restart loses nothing.
func (o *Orchestrator) quarSettle(ctx context.Context, d store.NodeDisk) error {
	rec, ok, err := o.quarLoad(ctx, d.ID)
	if err != nil || !ok {
		return err
	}
	if rec.Jobs == nil {
		rec.Jobs = map[string]string{}
	}
	if rec.Tries == nil {
		rec.Tries = map[string]int{}
	}
	var tiles, members []string
	stuck := false
	var failed []error
	for _, id := range rec.Tiles {
		keep, st, err := o.settleTile(ctx, &rec, id)
		if err != nil {
			failed = append(failed, err)
		}
		if keep {
			tiles = append(tiles, id)
		}
		stuck = stuck || st
	}
	for _, id := range rec.Members {
		keep, st, err := o.settleMember(ctx, &rec, id)
		if err != nil {
			failed = append(failed, err)
		}
		if keep {
			members = append(members, id)
		}
		stuck = stuck || st
	}
	if len(tiles)+len(members) == 0 {
		o.clearProblem(ctx, quarProblemKey(d.ID))
		return errors.Join(append(failed, o.store.Settings.Delete(ctx, quarKey(d.ID)))...)
	}
	if stuck {
		o.openProblem(ctx, quarProblemKey(d.ID), SevWarning, "Something stopped by the disk problem did not come back. Deploy it again, or press Check disk.")
	} else {
		o.clearProblem(ctx, quarProblemKey(d.ID))
	}
	rec.Tiles, rec.Members = tiles, members
	return errors.Join(append(failed, o.quarSave(ctx, d.ID, rec))...)
}

func (o *Orchestrator) settleTile(ctx context.Context, rec *quarRec, id string) (keep, stuck bool, err error) {
	t, err := o.tiles.Get(ctx, id)
	if errors.Is(err, errs.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return true, false, err
	}
	if t.Paused {
		return false, false, nil
	}
	if stopped, err := o.stoppedByUser(ctx, t); err != nil {
		return true, false, err
	} else if stopped {
		return false, false, nil
	}
	newest, has, err := o.newestStart(ctx, t)
	if err != nil {
		return true, false, err
	}
	if has && job.Live(newest.State) {
		return true, false, nil // its job is on the way
	}
	if o.tileRunning(ctx, t) && (!has || newest.State == job.Done) {
		return false, false, nil
	}
	key := "t:" + id
	if rec.Tries[key] >= quarTries {
		return true, true, nil
	}
	if reach, err := o.reachedByRedeploy(ctx, t); err != nil {
		return true, false, err
	} else if !reach {
		return false, false, nil
	}
	j, err := o.Deploy(ctx, id)
	if err != nil {
		return true, false, fmt.Errorf("%s: %w", t.Slug, err) // refused (the disk failed again): tried at the next read
	}
	rec.Jobs[key] = j.ID
	rec.Tries[key]++
	return true, false, nil
}

func (o *Orchestrator) settleMember(ctx context.Context, rec *quarRec, id string) (keep, stuck bool, err error) {
	m, err := o.members.Get(ctx, id)
	if errors.Is(err, errs.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return true, false, err
	}
	if m.State != opmember.Running || opmember.MayStart(m) != nil {
		return false, false, nil
	}
	if up, err := o.memberRunning(ctx, m); err != nil {
		return true, false, err
	} else if up {
		return false, false, nil
	}
	key := "m:" + id
	if rec.Tries[key] >= quarTries {
		return true, true, nil
	}
	rec.Tries[key]++
	if err := o.startMember(ctx, m); err != nil {
		return true, false, fmt.Errorf("start member %s: %w", id, err)
	}
	return true, false, nil
}

func (o *Orchestrator) quarSettleQuiet(ctx context.Context, d store.NodeDisk) {
	if err := o.quarSettle(ctx, d); err != nil {
		slog.Warn("disk quarantine: settle", "disk", d.ID, "err", err)
	}
}
