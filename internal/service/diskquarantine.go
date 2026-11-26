package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
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
}

// quarRetry is how long a failed quarantine waits before the watch queues it again.
const quarRetry = 5 * time.Minute

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
			if slices.Contains(rec.Tiles, t.ID) || o.tileRunning(ctx, t) {
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
		if slices.Contains(rec.Members, m.ID) || o.memberRunning(ctx, m) {
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
func (o *Orchestrator) memberRunning(ctx context.Context, m InstanceMember) bool {
	cs, err := o.docker.List(docker.WithNode(ctx, m.NodeID), map[string]string{LabelMember: m.ID})
	return err == nil && slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" || c.State == "restarting" })
}

// dropFromSync runs the instance's sync pass (under its topology lock), which
// leaves out a standby whose disk is missing, so the primary stops waiting for
// it and the pass clears the member's sync_in flag. A pass that cannot run (busy,
// failing over) or fails while the primary is running is an error: the standby
// stays up and the job runs again. A primary that is not running waits for
// nobody, so the stop goes on.
func (o *Orchestrator) dropFromSync(ctx context.Context, instanceID string) error {
	inst, err := o.operated.Get(ctx, instanceID)
	if err != nil {
		return err
	}
	t, err := o.tiles.Get(ctx, inst.TileID)
	if err != nil {
		return err
	}
	if err := o.syncInstance(ctx, t, inst); err != nil && o.tileRunning(ctx, t) {
		return err
	}
	return nil
}

// resumeQuarantined restarts exactly what the quarantine of d stopped (the
// tiles by a deploy, the standbys by their own start, which keeps their rows)
// and forgets the record. With no record it is Check disk's plain redeploy of
// whatever is not running.
func (o *Orchestrator) resumeQuarantined(ctx context.Context, r *jobs.Run, d store.NodeDisk) error {
	rec, ok, err := o.quarLoad(ctx, d.ID)
	if err != nil {
		return err
	}
	var only map[string]bool
	if ok {
		only = map[string]bool{}
		for _, id := range rec.Tiles {
			only[id] = true
		}
	}
	failed := []error{o.deployDiskTiles(ctx, r, d, only)}
	for _, id := range rec.Members {
		m, err := o.members.Get(ctx, id)
		if err != nil {
			continue // removed since
		}
		if m.State != opmember.Running || opmember.MayStart(m) != nil {
			continue
		}
		if err := o.startMember(ctx, m); err != nil {
			failed = append(failed, fmt.Errorf("start member %s: %w", id, err))
			continue
		}
		_, _ = fmt.Fprintf(r.Log, "started member %s\n", id)
	}
	if ok {
		if err := o.store.Settings.Delete(ctx, quarKey(d.ID)); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// memberDiskDown: standby m's data folder is on a missing disk. The tick's
// startMembers must not bring it back before Check disk has.
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

// DiskErroring is whether the node's ready disk reads as erroring now (the
// Nodes list shows it as Disk failed, like a missing one).
func (o *Orchestrator) DiskErroring(nodeID string) bool { return o.diskErroring(nodeID) }

// failedDisk is the node's disk when it is missing or erroring.
func (o *Orchestrator) failedDisk(ctx context.Context, nodeID string) (store.NodeDisk, bool) {
	d, has, err := o.nodeDisk(ctx, nodeID)
	if err != nil || !has {
		return d, false
	}
	return d, d.State == disk.StateMissing || (d.State == disk.StateReady && o.diskErroring(nodeID))
}

// failedDiskGate refuses a deploy of env e when its node's disk has failed and
// the env has a volume on it or would create one. A stateless env passes.
func (o *Orchestrator) failedDiskGate(ctx context.Context, e store.Environment) error {
	d, failed := o.failedDisk(ctx, e.NodeID)
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
		if found, _, err := o.docker.VolumeDevice(nctx, v.Name); err == nil && !found {
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

// erroringDiskGate refuses new data on a node whose disk reads as erroring:
// a move of volumes or an env onto it. Missing is the volume leaf's to refuse.
func (o *Orchestrator) erroringDiskGate(ctx context.Context, nodeID string) error {
	if nodeID == "" || !o.diskErroring(nodeID) {
		return nil
	}
	if d, has, err := o.nodeDisk(ctx, nodeID); err != nil || !has || d.State != disk.StateReady {
		return err
	}
	return errs.Refusedf("The disk on %s is failing. Check the disk first.", o.nodeName(ctx, nodeID))
}

// tileDiskDown: tile t has a volume on a missing disk of its node.
func (o *Orchestrator) tileDiskDown(ctx context.Context, t store.Tile) bool {
	e, err := o.store.Environments.Get(ctx, t.EnvironmentID)
	if err != nil || e.NodeID == "" {
		return false
	}
	d, has, err := o.nodeDisk(ctx, e.NodeID)
	if err != nil || !has || d.State != disk.StateMissing {
		return false
	}
	fs, err := o.diskLeaf.Folders(ctx, d)
	if err != nil {
		return false
	}
	on := map[string]bool{}
	for _, f := range fs {
		on[f.Name] = true
	}
	ts, err := o.diskTiles(ctx, e, on)
	return err == nil && slices.ContainsFunc(ts, func(x Tile) bool { return x.ID == t.ID })
}
