package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Move to disk: one volume, or one group of Move all (a connected component
// of volumes and the tiles that mount them), copied onto the node's disk.
//
// Crash model: a job that was running at a crash is failed at start, never
// resumed. A second click recomputes the same target (derived from the volume
// id) and branches on the row: the row names the target and the disk has its
// folder row = the swap committed, only the deploy is left; any other state
// is a fresh move that discards a partial copy and copies again.

const auditVolumeToDisk = "volume.to_disk"

// moveTarget is the disk-backed name of a volume. Fixed, never generated per click.
func moveTarget(volumeID string) string { return "stackr-vol-" + volumeID + "-disk" }

// moveTargetOf is the name a volume moves to: the fixed target, or, for a
// plain volume that already carries a target name but has no folder (after a
// Forget, or an env moved to a diskless node), the other of the two, so it is
// a fresh move and not a resume. A name with a folder row is a finished swap
// and stays.
func (o *Orchestrator) moveTargetOf(ctx context.Context, d store.NodeDisk, v Volume) (string, error) {
	base := moveTarget(v.ID)
	if v.Name != base && v.Name != base+"2" {
		return base, nil
	}
	has, err := o.diskLeaf.HasFolders(ctx, d, v.Name)
	if err != nil {
		return "", err
	}
	switch {
	case has[v.Name]:
		return v.Name, nil
	case v.Name == base:
		return base + "2", nil
	}
	return base, nil
}

// isMoveName is whether name is one of the two names a volume moves to.
func isMoveName(volumeID, name string) bool {
	return name == moveTarget(volumeID) || name == moveTarget(volumeID)+"2"
}

// todiskKey serialises the volume-to-disk jobs of one node, so each group's
// tiles are down only during its own copy (2N holds it for every group).
func todiskKey(nodeID string) string { return "todisk:" + nodeID }

// volumeMoveItem is one volume of a move: its derived target and the name it
// has now, kept in the payload because no label identifies the old copy.
type volumeMoveItem struct {
	VolumeID string `json:"volume_id"`
	Target   string `json:"target"`
	OldName  string `json:"old_name"`
}

// volumeMoveJob is the volume-to-disk payload: a group (one for the single verb).
type volumeMoveJob struct {
	NodeID  string           `json:"node_id"`
	Volumes []volumeMoveItem `json:"volumes"`
	By      string           `json:"by,omitempty"`
	KeyID   string           `json:"key_id,omitempty"`
}

// MoveVolumeToDisk is the single verb (route verb tile.write): the group
// core with a list of one.
func (o *Orchestrator) MoveVolumeToDisk(ctx context.Context, by *Principal, volumeID string) (Job, error) {
	v, err := o.volumes.Get(ctx, volumeID)
	if err != nil {
		return Job{}, err
	}
	return o.enqueueVolumeMove(ctx, by, o.volumeNode(ctx, v.ID), []Volume{v})
}

// enqueueVolumeMove is the group core's front door (2N calls it once per
// group): it judges vs, the volumes of one group on nodeID, refuses what
// cannot move and queues one volume-to-disk job whose lock set is the volume
// keys, the tiles mounting them, the env config lock and the node's todisk
// key. Never nodeLock: deploys of other envs on the node keep working.
func (o *Orchestrator) enqueueVolumeMove(ctx context.Context, by *Principal, nodeID string, vs []Volume) (Job, error) {
	if _, err := o.diskNodeUp(ctx, nodeID); err != nil {
		return Job{}, err
	}
	if err := o.erroringDiskGate(ctx, nodeID); err != nil {
		return Job{}, err
	}
	if err := o.moveLive(ctx, vs); err != nil {
		return Job{}, err
	}
	d, has, err := o.nodeDisk(ctx, nodeID)
	if err != nil {
		return Job{}, err
	}
	uid, kid := actorOf(by)
	p := volumeMoveJob{NodeID: nodeID, By: uid, KeyID: kid}
	for _, v := range vs {
		target := moveTarget(v.ID) // load refuses a node with no ready disk
		if has {
			if target, err = o.moveTargetOf(ctx, d, v); err != nil {
				return Job{}, err
			}
		}
		if has && d.Kind == disk.KindXFS && v.MaxSizeMB != nil && o.volumes.SizesFresh(ctx, []Volume{v})[v.ID] > capBytes(v.MaxSizeMB) {
			return Job{}, errs.Refusedf("The volume holds more than its cap. Raise the cap first.")
		}
		p.Volumes = append(p.Volumes, volumeMoveItem{VolumeID: v.ID, Target: target, OldName: v.Name})
	}
	m := &volumeMove{o: o, p: p}
	if err := m.load(ctx, true); err != nil {
		return Job{}, err
	}
	keys := make([]string, len(m.vs))
	for i, v := range m.vs {
		keys[i] = "volume:" + v.ID
	}
	return o.enqueueExclusive(ctx, kindVolumeToDisk, p, keys, errs.Conflictf("This volume is already moving to the disk."), m.lock()...)
}

// moveLive refuses while a live volume-to-disk job holds any of vs: a second
// press never cancels a running move.
func (o *Orchestrator) moveLive(ctx context.Context, vs []Volume) error {
	live, err := o.jobRows.List(ctx, job.Queued, job.Waiting, job.Running)
	if err != nil {
		return err
	}
	for _, j := range live {
		var p volumeMoveJob
		if j.Kind != string(kindVolumeToDisk) || json.Unmarshal([]byte(j.Payload), &p) != nil {
			continue
		}
		for _, it := range p.Volumes {
			if slices.ContainsFunc(vs, func(v Volume) bool { return v.ID == it.VolumeID }) {
				return errs.Conflictf("This volume is already moving to the disk.")
			}
		}
	}
	return nil
}

func (o *Orchestrator) runVolumeToDisk(ctx context.Context, r *jobs.Run, p volumeMoveJob) error {
	return (&volumeMove{o: o, r: r, p: p}).run(ctx)
}

// moveStop is what a test's hook returns to end a run as a crash would: no
// cleanup runs after it.
type moveStop struct{ error }

func (s moveStop) Unwrap() error { return s.error }

// volumeMove is one run of the group core.
type volumeMove struct {
	o     *Orchestrator
	r     *jobs.Run
	p     volumeMoveJob
	d     store.NodeDisk
	vs    []Volume                // the rows, in payload order
	skip  []bool                  // true = resume: the swap committed, only the deploy is left
	tiles []Tile                  // every tile that mounts or owns one of vs
	after func(step string) error // a test's way to stop the run after a step
}

func (m *volumeMove) step(name string) error {
	if m.after == nil {
		return nil
	}
	if err := m.after(name); err != nil {
		return moveStop{err}
	}
	return nil
}

func (m *volumeMove) logf(format string, a ...any) {
	if m.r != nil && m.r.Log != nil {
		_, _ = fmt.Fprintf(m.r.Log, format+"\n", a...)
	}
}

// lock is the job's lock set.
func (m *volumeMove) lock() []string {
	l := []string{todiskKey(m.p.NodeID)}
	for _, v := range m.vs {
		l = append(l, "volume:"+v.ID)
		if k := "env:" + v.ScopeID; !slices.Contains(l, k) {
			l = append(l, k)
		}
	}
	for _, t := range m.tiles {
		l = append(l, t.ID)
	}
	return l
}

// load reads and judges the group: every refusal of the move. queued is the
// verb's look before the job exists; the job's own look (queued false) is
// after its locks are held, so a lock of its own is not "busy" and the set
// of mounting tiles must still be inside the lock set.
func (m *volumeMove) load(ctx context.Context, queued bool) error {
	o, p := m.o, m.p
	if len(p.Volumes) == 0 {
		return errs.Refusedf("Nothing to move.")
	}
	d, has, err := o.nodeDisk(ctx, p.NodeID)
	if err != nil {
		return err
	}
	if !has || d.State != disk.StateReady {
		return errs.Refusedf("This node's disk is not ready.")
	}
	m.d = d
	m.vs, m.skip = nil, nil
	var fresh []Volume
	for _, it := range p.Volumes {
		v, err := o.volumes.Get(ctx, it.VolumeID)
		if err != nil {
			return err
		}
		if want, err := o.moveTargetOf(ctx, d, v); err != nil {
			return err
		} else if it.Target != want {
			return errs.Refusedf("The move names the wrong disk volume.")
		}
		if v.ScopeKind != "env" || v.OrphanedAt != nil {
			return errs.Refusedf("Only the volumes of an env move to the disk.")
		}
		if n := o.volumeNode(ctx, v.ID); n != p.NodeID {
			return errs.Conflictf("The volume is not on this node.")
		}
		resume, err := o.moveResumes(ctx, d, v, it.Target)
		if err != nil {
			return err
		}
		if !resume {
			if v.Name != it.OldName {
				return errs.Conflictf("The volume changed, try again.")
			}
			if v.InstanceID != nil {
				if err := o.noStandbys(ctx, *v.InstanceID); err != nil {
					return err
				}
				if err := o.notRelocated(ctx, *v.InstanceID); err != nil {
					return err
				}
			}
			var except []string
			if !queued {
				except = []string{string(kindVolumeToDisk)}
			}
			if err := o.volumeIdle(ctx, v, except...); err != nil {
				return err
			}
			fresh = append(fresh, v)
		}
		m.vs, m.skip = append(m.vs, v), append(m.skip, resume)
	}
	if m.tiles, err = o.moveTiles(ctx, m.vs); err != nil {
		return err
	}
	if !queued && m.r != nil {
		for _, t := range m.tiles {
			if !slices.Contains(m.r.Job.LockSet, t.ID) {
				return errs.Conflictf("The volume's mounts changed, try again.")
			}
		}
	}
	return m.room(ctx, fresh)
}

// moveResumes branches on the row before any refusal about "already on the
// disk": true = the row names the target and the disk has its folder row, so
// the copy is done. Any other row or member naming the target is refused.
func (o *Orchestrator) moveResumes(ctx context.Context, d store.NodeDisk, v Volume, target string) (bool, error) {
	live, err := o.store.Volumes.ListLive(ctx)
	if err != nil {
		return false, err
	}
	orphans, err := o.store.Volumes.ListOrphaned(ctx)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(append(live, orphans...), func(x Volume) bool { return x.ID != v.ID && x.Name == target }) {
		return false, errs.Conflictf("Another volume already uses the name this volume would get on the disk.")
	}
	ms, err := o.members.All(ctx)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(ms, func(x store.OperatedMember) bool { return x.Volume == target }) {
		return false, errs.Conflictf("A standby already uses the name this volume would get on the disk.")
	}
	if v.Name == target {
		has, err := o.diskLeaf.HasFolders(ctx, d, target)
		if err != nil {
			return false, err
		}
		if !has[target] {
			return false, errs.Refusedf("The volume is named for the disk but has no folder there.")
		}
		// a finished swap with the tiles up is not a resume: every press would redeploy
		if half, err := o.halfMoved(ctx, v); err != nil {
			return false, err
		} else if !half {
			return false, errs.Refusedf("Already on the disk.")
		}
		// The deploy must not start on a folder that is gone, or empty while the
		// old copy still holds data: a missing target is never recreated here.
		use, err := o.diskLeaf.Usage(ctx, d, true, target)
		if err != nil {
			return false, err
		}
		u := use[target]
		if u.Missing {
			return false, errs.Refusedf("The disk copy of %s is missing. The old copy is kept with the orphans: restore the folder or the old copy first.", v.Slug)
		}
		if u.Empty {
			orphans, err := o.store.Volumes.ListOrphaned(ctx)
			if err != nil {
				return false, err
			}
			for _, x := range orphans {
				if x.ScopeID == v.ScopeID && x.NodeID == d.NodeID && strings.HasPrefix(x.Slug, v.Slug+"-") && o.volumes.HoldsData(ctx, x) {
					return false, errs.Refusedf("The disk copy of %s is empty but the old copy holds data. It is kept with the orphans: restore it first.", v.Slug)
				}
			}
		}
		return true, nil
	}
	if o.volumes.OnDisk(ctx, v) {
		return false, errs.Refusedf("The volume is already on the disk.")
	}
	return false, nil
}

// notRelocated refuses an instance whose primary runs off its env's node
// after a failover: its data is not on this node, and a move would remove
// the volume and then be refused its deploy.
func (o *Orchestrator) notRelocated(ctx context.Context, instanceID string) error {
	it, err := o.instanceTile(ctx, instanceID)
	if err != nil {
		return err
	}
	if _, ok := o.relocated(ctx, it.ID); ok {
		return errs.Refusedf("%s runs its primary on another node after a failover. Move it back first.", it.Name)
	}
	return nil
}

// room refuses unless the fresh volumes, plus a tenth, fit on the disk.
// A size Docker cannot tell counts as nothing (as the postgres change does).
func (m *volumeMove) room(ctx context.Context, fresh []Volume) error {
	if len(fresh) == 0 {
		return nil
	}
	var sum int64
	for _, s := range m.o.volumes.SizesFresh(ctx, fresh) {
		sum += max(s, 0)
	}
	need := sum + sum/10
	free, _, err := m.o.dataDiskOf(ctx, m.p.NodeID)
	if err != nil {
		return err
	}
	if int64(free) < need {
		return errs.Conflictf("The disk has %s free and the move needs %s (the data and a tenth over).", gb(free), gb(uint64(need)))
	}
	return nil
}

// moveTiles are the tiles that mount, or own, any of vs, once each.
func (o *Orchestrator) moveTiles(ctx context.Context, vs []Volume) ([]Tile, error) {
	var out []Tile
	seen := map[string]bool{}
	for _, v := range vs {
		ts, err := o.mounters(ctx, v)
		if err != nil {
			return nil, err
		}
		if v.InstanceID != nil {
			it, err := o.instanceTile(ctx, *v.InstanceID)
			if err != nil {
				return nil, err
			}
			ts = append(ts, it)
		}
		for _, t := range ts {
			if !seen[t.ID] {
				seen[t.ID] = true
				out = append(out, t)
			}
		}
	}
	return out, nil
}

// run is steps 1 to 10 of the move.
func (m *volumeMove) run(ctx context.Context) error {
	if err := m.load(ctx, false); err != nil {
		return err
	}
	var fresh []int
	for i, resume := range m.skip {
		if !resume {
			fresh = append(fresh, i)
		}
	}
	if len(fresh) > 0 {
		if err := m.removeContainers(ctx); err != nil {
			return m.abort(ctx, err, nil, -1, nil)
		}
		if err := m.step("containers"); err != nil {
			return err
		}
		for k, i := range fresh {
			if err := m.copyOne(ctx, i); err != nil {
				return m.abort(ctx, err, fresh[:k], i, fresh[k+1:])
			}
		}
		if err := m.swap(ctx, fresh); err != nil {
			return m.abort(ctx, err, nil, -1, nil)
		}
		if err := m.step("swap"); err != nil {
			return err
		}
	}
	if err := m.deploy(ctx); err != nil {
		return err
	}
	all := make([]int, len(m.p.Volumes))
	for i := range all {
		all[i] = i
	}
	if err := m.release(ctx, all); err != nil {
		return err
	}
	return m.step("deploy")
}

// removeContainers removes the mounting tiles' containers (idempotent: only
// what is there). A stopped container would still hold the old volume.
func (m *volumeMove) removeContainers(ctx context.Context) error {
	var failed []error
	for _, t := range m.tiles {
		tctx, err := m.o.withTileNode(ctx, t.ID)
		if err != nil {
			return err
		}
		cs, err := m.o.tiles.Replicas(tctx, t)
		if err != nil {
			return err
		}
		for _, c := range cs {
			m.logf("removing %s", c.Name)
			if err := m.o.tiles.Remove(tctx, t.ID, c.ID); err != nil {
				failed = append(failed, fmt.Errorf("%s: %w", c.Name, err))
			}
		}
	}
	return errors.Join(failed...)
}

// copyOne is steps 6 to 8 for volume i: discard a partial target, make it,
// copy the old volume into it, check it.
func (m *volumeMove) copyOne(ctx context.Context, i int) error {
	o, v, target := m.o, m.vs[i], m.p.Volumes[i].Target
	nctx := docker.WithNode(ctx, m.p.NodeID)
	m.logf("copying %s to the disk", v.Slug)
	// an earlier copy died before the swap: the row still names the old
	// volume, so this only discards a partial copy
	if err := o.volumes.RemoveNamed(nctx, m.p.NodeID, target); err != nil {
		return err
	}
	if err := o.volumes.EnsureNamed(nctx, m.p.NodeID, target, map[string]string{"stackr.volume": v.ID}); err != nil {
		return err
	}
	has, err := o.diskLeaf.HasFolders(ctx, m.d, target)
	if err != nil {
		return err
	}
	if found, dev, err := o.docker.VolumeDevice(nctx, target); err != nil {
		return err
	} else if !found || dev == "" || !has[target] {
		return errs.Conflictf("%s did not come up on the disk.", v.Slug)
	}
	if err := m.step("ensure"); err != nil {
		return err
	}
	dst := v
	dst.Name = target
	if err := o.volumes.EnsureTool(ctx, v); err != nil {
		return err
	}
	if err := pipeVolume(ctx, o, v, dst); err != nil {
		return err
	}
	if err := m.step("copy"); err != nil {
		return err
	}
	// HoldsData fails safe (unknown counts as data), which is wrong for a
	// check: the copy must be proven present, so ask the disk and fail on any error.
	use, err := o.diskLeaf.Usage(ctx, m.d, true, target)
	if err != nil {
		return fmt.Errorf("checking the copy of %s: %w", v.Slug, err)
	}
	if u := use[target]; u.Missing || (u.Empty && o.volumes.HoldsData(ctx, v)) {
		return errs.Conflictf("The copy of %s is missing or empty.", v.Slug)
	}
	return m.step("check")
}

// pipeVolume tars src into an untar of dst. The untar wipes dst first, so a
// repeat after a crash starts clean.
func pipeVolume(ctx context.Context, o *Orchestrator, src, dst Volume) error {
	pr, pw := io.Pipe()
	tarErr := make(chan error, 1)
	go func() {
		err := o.volumes.Tar(ctx, src, pw, false)
		_ = pw.CloseWithError(err) // nil = EOF
		tarErr <- err
	}()
	uerr := o.volumes.Untar(ctx, dst, pr)
	_ = pr.CloseWithError(io.ErrClosedPipe) // unblock the tar side if the untar stopped early
	if terr := <-tarErr; terr != nil {
		return terr // the cause; an untar error after it is its echo
	}
	return uerr
}

// moveHold is the orphaned_at of an old copy whose move has not deployed yet:
// retention (older than N days) never reaches it, so the orphan job cannot
// back up and delete the only good copy while the disk copy is unproven.
// release starts the clock once the deploy succeeded.
// ponytail: an env deleted while held keeps the hold (the delete trigger keeps
// orphaned_at), so that old copy never expires; an admin deletes it by hand.
var moveHold = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// release starts retention on the old copies of the items idx, once their
// deploy succeeded. Only rows still held are touched.
func (m *volumeMove) release(ctx context.Context, idx []int) error {
	orphans, err := m.o.store.Volumes.ListOrphaned(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, i := range idx {
		for _, x := range orphans {
			if x.Name == m.p.Volumes[i].OldName && x.ScopeID == m.vs[i].ScopeID && x.ScopeKind == m.vs[i].ScopeKind &&
				x.OrphanedAt != nil && x.OrphanedAt.Equal(moveHold) {
				x.OrphanedAt = &now
				if err := m.o.store.Volumes.Update(ctx, x); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// swap is step 9, one transaction for the volumes copied: the row names the
// target, the old name goes onto an orphaned row the orphan job removes
// (the swapData pattern). A relocated primary never moves, so no member row follows.
func (m *volumeMove) swap(ctx context.Context, idx []int) error {
	if len(idx) == 0 {
		return nil
	}
	now := time.Now().UTC()
	err := m.o.store.Tx(ctx, func(tx store.Tx) error {
		for _, i := range idx {
			it := m.p.Volumes[i]
			cur, err := tx.Volumes.Get(ctx, it.VolumeID)
			if err != nil {
				return err
			}
			if cur.Name != it.OldName {
				return errs.Conflictf("The volume changed, try again.")
			}
			id := uuid.NewString()
			if err := tx.Volumes.Create(ctx, store.Volume{
				ID: id, ScopeKind: cur.ScopeKind, ScopeID: cur.ScopeID, Slug: cur.Slug + "-" + id[:8],
				Name: it.OldName, OrphanedAt: &moveHold, CreatedAt: now, NodeID: m.p.NodeID,
			}); err != nil {
				return err
			}
			cur.Name = it.Target
			if err := tx.Volumes.Update(ctx, cur); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, i := range idx {
		m.logf("%s is on the disk; the old copy goes with the orphans", m.vs[i].Slug)
		uid, kid := m.p.By, m.p.KeyID
		if err := securitylog.New(m.o.store.SecurityEvents).Append(ctx, securitylog.Event{
			Action: auditVolumeToDisk, ActorID: uid, KeyID: kid, TargetKind: "volume", TargetID: m.vs[i].ID,
			Detail: "from=" + m.p.Volumes[i].OldName,
		}); err != nil {
			slog.Error("audit", "action", auditVolumeToDisk, "error", err.Error())
		}
	}
	return nil
}

// deploy is step 10: the mounting tiles deploy inline, as the deploy job
// does. A queued deploy would wait on the tile locks this job holds. A tile
// the user stopped or paused stays down.
func (m *volumeMove) deploy(ctx context.Context) error {
	var failed []error
	for _, t := range m.tiles {
		t, err := m.o.tiles.Get(ctx, t.ID)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		if t.Paused || t.Kind == tile.Slice || tile.RunToCompletion(t.Kind) {
			continue
		}
		if stopped, err := m.o.stoppedByUser(ctx, t); err != nil {
			failed = append(failed, err)
			continue
		} else if stopped {
			continue
		}
		m.logf("deploying %s", t.Slug)
		b, _ := json.Marshal(tileJob{TileID: t.ID})
		run := &jobs.Run{Job: store.Job{Payload: string(b), CreatedAt: m.r.Job.CreatedAt, ActorID: m.r.Job.ActorID, ActorKeyID: m.r.Job.ActorKeyID}, Log: m.r.Log}
		if err := m.o.handlers()[kindDeploy](ctx, run); err != nil {
			failed = append(failed, fmt.Errorf("%s did not start on the new copy: %w", t.Slug, err))
		}
	}
	return errors.Join(failed...)
}

// abort is a failure before the swap (or of it): the volume that failed loses
// its partial target, the ones never attempted are logged as skipped, the
// ones already copied are swapped (their copies were checked), and the tiles
// deploy once, on that mixed old and new set. The old copies are removed only
// through the orphan rows, after that deploy. It runs on a context that is
// not cancelled, so a cancelled job still leaves the tiles up.
func (m *volumeMove) abort(ctx context.Context, cause error, copied []int, failed int, rest []int) error {
	var stop moveStop
	if errors.As(cause, &stop) {
		return cause // a simulated crash: nothing runs after it
	}
	ctx = context.WithoutCancel(ctx)
	errList := []error{cause}
	if failed >= 0 {
		nctx := docker.WithNode(ctx, m.p.NodeID)
		if err := m.o.volumes.RemoveNamed(nctx, m.p.NodeID, m.p.Volumes[failed].Target); err != nil {
			errList = append(errList, err)
		}
	}
	for _, i := range rest {
		m.logf("skipped %s", m.vs[i].Slug)
	}
	if err := m.swap(ctx, copied); err != nil {
		errList = append(errList, err)
	}
	if err := m.deploy(ctx); err != nil {
		errList = append(errList, err)
	} else if err := m.release(ctx, copied); err != nil {
		errList = append(errList, err)
	}
	return errors.Join(errList...)
}

// VolumeMoveInfo is what a volume drawer's Stored on card offers. Rows only,
// so the drawer may ask it on every poll.
type VolumeMoveInfo struct {
	Offer   bool   // a Move to disk (or Finish move) button is drawn
	Why     string // the button is disabled and this says why; "" with Running = disabled, no hint
	Finish  bool   // an earlier move swapped the volume and was cut off: the button finishes it
	Running bool   // a volume-to-disk job holds the volume
}

// VolumeMoveInfo reads what the volume's card offers.
func (o *Orchestrator) VolumeMoveInfo(ctx context.Context, volumeID string) (VolumeMoveInfo, error) {
	var m VolumeMoveInfo
	v, err := o.volumes.Get(ctx, volumeID)
	if err != nil || v.ScopeKind != ScopeEnv {
		return m, err
	}
	nodeID := o.volumeNode(ctx, v.ID)
	d, has, err := o.nodeDisk(ctx, nodeID)
	if err != nil || !has {
		return m, err
	}
	m.Running = o.moveLive(ctx, []Volume{v}) != nil
	if o.volumes.OnDisk(ctx, v) {
		if isMoveName(v.ID, v.Name) {
			m.Finish, err = o.lastMoveFailed(ctx, v.ID)
			m.Offer = m.Finish
		}
		return m, err
	}
	m.Offer = true
	switch {
	case d.State != disk.StateReady:
		m.Why = "The node disk is not ready."
	case v.InstanceID != nil && o.noStandbys(ctx, *v.InstanceID) != nil:
		m.Why = "This instance has standbys. Remove them first."
	default:
		// the plan's own refusal: a disk nobody has read cannot be sized against
		if _, _, err := o.dataDiskOf(ctx, nodeID); errors.As(err, new(errs.Refused)) {
			m.Why = err.Error()
		}
	}
	return m, nil
}

// lastMoveFailed is whether the newest volume-to-disk job that held the
// volume failed.
func (o *Orchestrator) lastMoveFailed(ctx context.Context, volumeID string) (bool, error) {
	js, err := o.jobRows.Recent(ctx, 200)
	if err != nil {
		return false, err
	}
	for _, j := range js { // newest first
		var p volumeMoveJob
		if j.Kind != string(kindVolumeToDisk) || json.Unmarshal([]byte(j.Payload), &p) != nil {
			continue
		}
		if slices.ContainsFunc(p.Volumes, func(it volumeMoveItem) bool { return it.VolumeID == volumeID }) {
			return j.State == job.Failed, nil
		}
	}
	return false, nil
}

// VolumeMovePlan is what the Move to disk dialog shows. SizeBytes is -1 when
// Docker cannot tell; Need is the data and a tenth over.
type VolumeMovePlan struct {
	Slug, Env string
	Tiles     []string // the tiles that stop while it copies
	SizeBytes int64
	Need      int64
	Free      uint64
	Fits      bool
	Finish    bool // only the deploy is left: nothing copies
}

// PlanVolumeMove measures the volume and the disk. It writes nothing; it asks
// Docker, so it answers an explicit press only, never a poll.
func (o *Orchestrator) PlanVolumeMove(ctx context.Context, by *Principal, volumeID string) (VolumeMovePlan, error) {
	v, err := o.volumes.Get(ctx, volumeID)
	if err != nil {
		return VolumeMovePlan{}, err
	}
	p := VolumeMovePlan{Slug: v.Slug, Env: o.moveEnvName(ctx, v), SizeBytes: -1}
	nodeID := o.volumeNode(ctx, v.ID)
	if _, err := o.diskNodeUp(ctx, nodeID); err != nil {
		return p, err
	}
	d, has, err := o.nodeDisk(ctx, nodeID)
	if err != nil {
		return p, err
	}
	if !has || d.State != disk.StateReady {
		return p, errs.Refusedf("This node's disk is not ready.")
	}
	ts, err := o.moveTiles(ctx, []Volume{v})
	if err != nil {
		return p, err
	}
	for _, t := range ts {
		p.Tiles = append(p.Tiles, t.Name)
	}
	if p.Free, _, err = o.dataDiskOf(ctx, nodeID); err != nil {
		return p, err
	}
	if o.volumes.OnDisk(ctx, v) { // a cut-off move: the copy is done
		p.Finish, p.Fits = true, true
		return p, nil
	}
	p.SizeBytes = o.volumes.SizesFresh(ctx, []Volume{v})[v.ID]
	p.Need = max(p.SizeBytes, 0) + max(p.SizeBytes, 0)/10
	p.Fits = uint64(p.Need) <= p.Free
	return p, nil
}

// OldCopyHeld is whether an orphaned_at stamp is the hold of a move's old
// copy: the panel then shows no date.
func OldCopyHeld(at time.Time) bool { return at.Equal(moveHold) }

// VolumeMoveJob is the newest volume-to-disk job that holds the volume: a
// live one, or one that ended in the last ten minutes. Rows only.
func (o *Orchestrator) VolumeMoveJob(ctx context.Context, volumeID string) (Job, bool, error) {
	js, err := o.Jobs(ctx, 200)
	if err != nil {
		return Job{}, false, err
	}
	for _, j := range js { // newest first
		var p volumeMoveJob
		if j.Kind != string(kindVolumeToDisk) || json.Unmarshal([]byte(j.Payload), &p) != nil {
			continue
		}
		if !slices.ContainsFunc(p.Volumes, func(it volumeMoveItem) bool { return it.VolumeID == volumeID }) {
			continue
		}
		if j.FinishedAt != nil && time.Since(*j.FinishedAt) > 10*time.Minute {
			return Job{}, false, nil
		}
		return j, true, nil
	}
	return Job{}, false, nil
}

// MoveJobTiles are the names of the tiles a volume-to-disk job stops while it
// copies. Rows only.
func (o *Orchestrator) MoveJobTiles(ctx context.Context, j Job) []string {
	var p volumeMoveJob
	if j.Kind != string(kindVolumeToDisk) || json.Unmarshal([]byte(j.Payload), &p) != nil {
		return nil
	}
	var vs []Volume
	for _, it := range p.Volumes {
		if v, err := o.volumes.Get(ctx, it.VolumeID); err == nil {
			vs = append(vs, v)
		}
	}
	ts, _ := o.moveTiles(ctx, vs)
	var out []string
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}
