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
	if ok { // the record stays until each redeploy and start is seen running
		failed = append(failed, o.quarSettle(ctx, d))
	}
	return errors.Join(failed...)
}

// memberDiskDown: standby m's data folder is on a missing disk. The tick's
// startMembers must not bring it back before Check disk has.
func (o *Orchestrator) memberDiskDown(ctx context.Context, m InstanceMember) bool {
	return m.Role == opmember.Replica && m.Volume != "" && o.folderDown(ctx, m.NodeID, m.Volume)
}

// DiskErroring is whether the node's ready disk reads as erroring now (the
// Nodes list shows it as Disk failed, like a missing one).
func (o *Orchestrator) DiskErroring(ctx context.Context, nodeID string) bool {
	return o.diskErroring(ctx, nodeID)
}

// failedDisk is the node's disk when it is missing or erroring.
func (o *Orchestrator) failedDisk(ctx context.Context, nodeID string) (store.NodeDisk, bool) {
	d, has, err := o.nodeDisk(ctx, nodeID)
	if err != nil || !has {
		return d, false
	}
	return d, d.State == disk.StateMissing || (d.State == disk.StateReady && o.diskErroring(ctx, nodeID))
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
	if nodeID == "" || !o.diskErroring(ctx, nodeID) {
		return nil
	}
	if d, has, err := o.nodeDisk(ctx, nodeID); err != nil || !has || d.State != disk.StateReady {
		return err
	}
	return errs.Refusedf("The disk on %s is failing. Check the disk first.", o.nodeName(ctx, nodeID))
}

// primaryDiskDown: the primary of an instance runs on node, and its data is
// on a missing disk there. After a failover that is the member's own volume on
// its node; before, the env's volumes on the env's node.
func (o *Orchestrator) primaryDiskDown(ctx context.Context, t store.Tile, m InstanceMember, node string) bool {
	if m.Volume != "" {
		return o.folderDown(ctx, node, m.Volume)
	}
	e, err := o.store.Environments.Get(ctx, t.EnvironmentID)
	if err != nil || e.NodeID == "" || e.NodeID != node {
		return false
	}
	d, has, err := o.nodeDisk(ctx, node)
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

// folderDown: folder name is on the missing disk of node.
func (o *Orchestrator) folderDown(ctx context.Context, node, name string) bool {
	d, has, err := o.nodeDisk(ctx, node)
	if err != nil || !has || d.State != disk.StateMissing {
		return false
	}
	fs, err := o.diskLeaf.Folders(ctx, d)
	return err == nil && slices.ContainsFunc(fs, func(f store.DiskFolder) bool { return f.Name == name })
}

// runGateTile is the storage gate for anything that starts the tile's
// containers (start, restart, a manual run, and a job that was queued before
// the disk failed): it refuses a tile whose env has data on a failed disk.
func (o *Orchestrator) runGateTile(ctx context.Context, tileID string) error {
	t, err := o.store.Tiles.Get(ctx, tileID)
	if err != nil {
		return nil
	}
	return o.runGateEnv(ctx, t.EnvironmentID)
}

func (o *Orchestrator) runGateEnv(ctx context.Context, envID string) error {
	e, err := o.store.Environments.Get(ctx, envID)
	if err != nil || e.NodeID == "" {
		return nil
	}
	return o.failedDiskGate(ctx, e)
}

// ---- the erroring latch, the check guard, the stopped set's settling

func erroringKey(diskID string) string { return "diskerroring." + diskID }

// diskErroring: the node's disk is latched erroring. The latch is a settings
// row, so it survives a panel restart; only Check disk clears it.
func (o *Orchestrator) diskErroring(ctx context.Context, nodeID string) bool {
	d, has, err := o.nodeDisk(ctx, nodeID)
	if err != nil || !has {
		return false
	}
	_, ok, err := o.store.Settings.Get(ctx, erroringKey(d.ID))
	return err == nil && ok
}

func (o *Orchestrator) latchErroring(ctx context.Context, d store.NodeDisk) {
	if _, ok, err := o.store.Settings.Get(ctx, erroringKey(d.ID)); err == nil && !ok {
		_ = o.store.Settings.Set(ctx, erroringKey(d.ID), time.Now().UTC().Format(time.RFC3339))
	}
}

func (o *Orchestrator) clearErroring(ctx context.Context, d store.NodeDisk) {
	_ = o.store.Settings.Delete(ctx, erroringKey(d.ID))
}

// diskGuard serializes Check disk's verify with the watch's latching of the
// same node's disk, and remembers when the last check finished: reads taken
// before that say nothing about the disk now.
type diskGuard struct {
	mu    sync.Mutex
	epoch atomic.Int64 // UnixNano
}

func (g *diskGuard) epochAt() time.Time {
	if n := g.epoch.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

func (o *Orchestrator) diskGuard(nodeID string) *diskGuard {
	g, _ := o.alertWatch().diskGuards.LoadOrStore(nodeID, &diskGuard{})
	return g.(*diskGuard)
}

// quarSettle prunes the stopped set of a resumed quarantine: a tile or standby
// that runs again (or is gone, or was stopped by the person since) is done.
// The record goes when nothing is pending; until then it keeps the rest, so
// a redeploy that has not finished, or a panel restart, loses nothing.
func (o *Orchestrator) quarSettle(ctx context.Context, d store.NodeDisk) error {
	rec, ok, err := o.quarLoad(ctx, d.ID)
	if err != nil || !ok {
		return err
	}
	var tiles, members []string
	for _, id := range rec.Tiles {
		t, err := o.tiles.Get(ctx, id)
		if errors.Is(err, errs.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if t.Paused || o.tileRunning(ctx, t) {
			continue
		}
		if stopped, err := o.stoppedByUser(ctx, t); err != nil {
			return err
		} else if stopped {
			continue
		}
		tiles = append(tiles, id)
	}
	for _, id := range rec.Members {
		m, err := o.members.Get(ctx, id)
		if errors.Is(err, errs.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if m.State != opmember.Running || o.memberRunning(ctx, m) {
			continue
		}
		members = append(members, id)
	}
	if len(tiles)+len(members) == 0 {
		return o.store.Settings.Delete(ctx, quarKey(d.ID))
	}
	if len(tiles) != len(rec.Tiles) || len(members) != len(rec.Members) {
		rec.Tiles, rec.Members = tiles, members
		return o.quarSave(ctx, d.ID, rec)
	}
	return nil
}

func (o *Orchestrator) quarSettleQuiet(ctx context.Context, d store.NodeDisk) {
	if err := o.quarSettle(ctx, d); err != nil {
		slog.Warn("disk quarantine: settle", "disk", d.ID, "err", err)
	}
}
