package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/upgrade"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/metrics"
)

// Move logs and metrics (the stackr-observe store) onto the controller's node
// disk and back (RFC 72bb, section 8). One job, run under observe.mu: only
// observe stops. The setting flips after the copy is proven, so a failed or
// crashed run leaves observe on its old volume, and a retry wipes the
// destination and copies again.

const (
	kindObserveMove jobs.Kind = "observe-move"

	// observeDataDiskKey is "1" while observe's data is on the node disk.
	observeDataDiskKey = "observe_data_disk"
	// observeDataCapKey is the xfs cap in bytes of the disk folder, read when
	// the folder is placed (folderCap).
	observeDataCapKey = "observe_data_cap"

	auditObserveToDisk   = "observe.to_disk"
	auditObserveFromDisk = "observe.from_disk"
)

// observeOnDisk is whether observe's data volume is the disk folder. A read
// error is an error, never "not on the disk": the caller must not change the
// running volume on a guess.
func (o *Orchestrator) observeOnDisk(ctx context.Context) (bool, error) {
	v, ok, err := o.store.Settings.Get(ctx, observeDataDiskKey)
	return err == nil && ok && v == "1", err
}

// ObserveOnDisk is whether logs and metrics live on the node disk: the Node
// disk card's row. A read of one setting.
func (o *Orchestrator) ObserveOnDisk(ctx context.Context) (bool, error) {
	return o.observeOnDisk(ctx)
}

// observeDiskRefusal is the refusal Forget (and Adopt) of a node's disk gives
// while observe's data is on it: move it back first. nil when it is not.
// forgetMissing is a Forget of a missing disk: the data is unreachable, so
// there is nothing to move back and the forget may go (forgetObserve).
func (o *Orchestrator) observeDiskRefusal(ctx context.Context, nodeID string, forgetMissing bool) error {
	if forgetMissing {
		return nil
	}
	if on, err := o.observeOnDisk(ctx); err != nil || !on {
		return err
	}
	if ctl, err := o.controllerID(ctx); err != nil || ctl != nodeID {
		return err
	}
	return errs.Refusedf("Logs and metrics are on this disk. Move them back to the system disk first.")
}

// forgetObserve is the part of a Forget of the controller's disk that frees
// observe from it: the settings go, the Docker object goes (best effort, the
// forget already removed the bound ones), and observe restarts on the plain
// volume. A no-op when observe is not on this node's disk.
func (o *Orchestrator) forgetObserve(ctx context.Context, nodeID string) error {
	if on, err := o.observeOnDisk(ctx); err != nil || !on {
		return err
	}
	if ctl, err := o.controllerID(ctx); err != nil || ctl != nodeID {
		return err
	}
	for _, k := range []string{observeDataDiskKey, observeDataCapKey} {
		if err := o.store.Settings.Delete(ctx, k); err != nil {
			return err
		}
	}
	_ = o.upgrade.RemoveObserve(ctx)
	_ = o.docker.RemoveVolume(docker.WithNode(ctx, nodeID), installspec.ObserveDiskVolume)
	o.resyncObserve(ctx, nodeID)
	return nil
}

// observeDiskReady is whether the controller has a ready node disk.
func (o *Orchestrator) observeDiskReady(ctx context.Context) bool {
	ctl, err := o.controllerID(ctx)
	if err != nil {
		return false
	}
	d, has, err := o.nodeDisk(ctx, ctl)
	if err != nil || !has || d.State != disk.StateReady {
		return false
	}
	bad, err := o.diskErroring(ctx, ctl)
	return err == nil && !bad
}

// observeRetryBase is the first wait between tries of resyncObserve; it doubles
// up to 30 s.
var observeRetryBase = time.Second

// resyncObserve reconciles observe with the controller's disk in the
// background, after the disk went missing or erroring (stop it) or came back
// (start it). It does not read any setting first: a failed read or ensure is
// retried with a growing wait, until observe is reconciled or the panel shuts
// down. A box that is not on the disk just gets an ordinary syncObserve.
func (o *Orchestrator) resyncObserve(ctx context.Context, nodeID string) {
	o.detach(context.Background(), func(ctx context.Context) {
		wait := observeRetryBase
		for range 12 {
			if o.reconcileObserve(ctx, nodeID) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(wait*2, 30*time.Second)
		}
		slog.Warn("observe: not reconciled with the node disk, giving up until the next change")
	})
}

// reconcileObserve is one try: true when nothing is left to do (observe is as
// the settings say, or it is rightly down because the disk is not ready, or
// the node is not the controller).
func (o *Orchestrator) reconcileObserve(ctx context.Context, nodeID string) bool {
	ctl, err := o.controllerID(ctx)
	if err != nil {
		return false
	}
	if ctl != nodeID {
		return true
	}
	cctx, err := o.withController(ctx)
	if err != nil {
		return false
	}
	o.syncObserve(cctx)
	e := o.observe.lastErr.Load()
	return e == nil || *e == "" || (*e == upgrade.ObserveDiskMissing && !o.observeDiskReady(ctx))
}

// observeCap is the xfs cap of the disk folder: twice today's data, and never
// under what the logs may grow to (their own size cap) plus a gigabyte for
// metrics plus the free space VictoriaMetrics keeps (observe_min_free), or the
// cap would stop observe before its own limits do.
// ponytail: the floor is read once at the move; editable later.
func (o *Orchestrator) observeCap(ctx context.Context, size int64) (uint64, error) {
	k, err := o.observeKnobs(ctx)
	if err != nil {
		return 0, err
	}
	floor := uint64(1<<30) + k.minFree
	if k.logs {
		floor += k.logsMax
	}
	return max(uint64(max(size, 0))*2, floor), nil
}

// ObserveMovePlan is what the move dialog shows before the confirm.
type ObserveMovePlan struct {
	NodeID    string
	Back      bool
	SizeBytes int64  // today's data; -1 = cannot tell
	Cap       uint64 // the folder's xfs cap on the disk (0 on the way back)
	Need      int64  // the copy and a tenth
	Free      uint64 // free bytes where the copy lands
	Fits      bool
	Busy      bool // a move holds the node's todisk key
}

func observeVolumeRow(name, nodeID string) Volume {
	return Volume{Name: name, NodeID: nodeID}
}

// PlanMoveObserve is read only (route verb node.manage). It is advisory: the
// job judges the move again when it runs.
func (o *Orchestrator) PlanMoveObserve(ctx context.Context, by *Principal, nodeID string, back bool) (ObserveMovePlan, error) {
	p := ObserveMovePlan{NodeID: nodeID, Back: back}
	if err := o.observeMoveLoad(ctx, nodeID, "", back); err != nil {
		return p, err
	}
	src := installspec.ObserveVolume
	if back {
		src = installspec.ObserveDiskVolume
	}
	p.SizeBytes = o.volumes.SizeBytesFresh(ctx, observeVolumeRow(src, nodeID))
	if p.SizeBytes < 0 {
		return p, errs.Refusedf("Cannot read the size of the logs and metrics. Try again.")
	}
	var err error
	if p.Free, err = o.observeDestFree(ctx, nodeID, back); err != nil {
		return p, err
	}
	if !back {
		if p.Cap, err = o.observeCap(ctx, p.SizeBytes); err != nil {
			return p, err
		}
	}
	p.Need = p.SizeBytes + p.SizeBytes/10
	p.Fits = uint64(p.Need) <= p.Free
	if !back && p.Fits {
		k, err := o.observeKnobs(ctx)
		if err != nil {
			return p, err
		}
		if left := p.Free - uint64(p.Need); left < k.minFree {
			return p, errs.Refusedf("The disk would have %s free after the move, under the %s that logs and metrics keep free.", gb(left), gb(k.minFree))
		}
	}
	if p.Busy, err = o.todiskLive(ctx, nodeID); err != nil {
		return p, err
	}
	return p, nil
}

// observeDestFree is the free space where the copy lands: the node disk, or
// on the way back the system disk.
func (o *Orchestrator) observeDestFree(ctx context.Context, nodeID string, back bool) (uint64, error) {
	if back {
		free, _ := o.diskOf(ctx, nodeID)
		return free, nil
	}
	free, _, err := o.dataDiskOf(ctx, nodeID)
	return free, err
}

// observeMoveLoad is every refusal of the move that does not depend on size.
func (o *Orchestrator) observeMoveLoad(ctx context.Context, nodeID, diskID string, back bool) error {
	if _, err := o.diskNodeUp(ctx, nodeID); err != nil {
		return err
	}
	if ctl, err := o.controllerID(ctx); err != nil {
		return err
	} else if ctl != nodeID {
		return errs.Refusedf("Logs and metrics live on the controller.")
	}
	d, has, err := o.nodeDisk(ctx, nodeID)
	if err != nil {
		return err
	}
	if !has || d.State != disk.StateReady {
		return errs.Refusedf("This node's disk is not ready.")
	}
	if diskID != "" && d.ID != diskID {
		return errs.Conflictf("The node's disk changed, try again.")
	}
	// moving data off a failing but readable disk is allowed
	if !back {
		if err := o.erroringDiskGate(ctx, nodeID); err != nil {
			return err
		}
	}
	on, err := o.observeOnDisk(ctx)
	if err != nil {
		return err
	}
	if on != back {
		if back {
			return errs.Refusedf("Logs and metrics are not on the disk.")
		}
		return errs.Refusedf("Logs and metrics are already on the disk.")
	}
	return nil
}

// observeMoveJob is the payload.
type observeMoveJob struct {
	NodeID string `json:"node_id"`
	DiskID string `json:"disk_id,omitempty"` // the disk whose lock the job holds
	Back   bool   `json:"back,omitempty"`
	By     string `json:"by,omitempty"`
	KeyID  string `json:"key_id,omitempty"`
}

// MoveObserve queues the move (route verb node.manage). Refused while a move
// of volumes holds the node's todisk key, like Move all.
func (o *Orchestrator) MoveObserve(ctx context.Context, by *Principal, nodeID string, back bool) (Job, error) {
	p, err := o.PlanMoveObserve(ctx, by, nodeID, back)
	if err != nil {
		return Job{}, err
	}
	if p.Busy {
		return Job{}, errs.Conflictf("Data is already moving to the disk.")
	}
	if !p.Fits {
		return Job{}, errs.Conflictf("%s has %s free and the move needs %s (the data and a tenth over).",
			map[bool]string{true: "The system disk", false: "The disk"}[back], gb(p.Free), gb(uint64(p.Need)))
	}
	d, _, err := o.nodeDisk(ctx, nodeID)
	if err != nil {
		return Job{}, err
	}
	uid, kid := actorOf(by)
	// the disk lock keeps a Forget or Adopt of this disk from overlapping the move
	return o.enqueueExclusive(ctx, kindObserveMove, observeMoveJob{NodeID: nodeID, DiskID: d.ID, Back: back, By: uid, KeyID: kid},
		[]string{"observe-move"}, errs.Conflictf("Logs and metrics are already moving."), "observe-move", todiskKey(nodeID), "disk:"+d.ID)
}

func (o *Orchestrator) runObserveMove(ctx context.Context, r *jobs.Run, p observeMoveJob) error {
	return (&observeMove{o: o, r: r, p: p}).run(ctx)
}

type observeMove struct {
	o *Orchestrator
	r *jobs.Run
	p observeMoveJob
}

func (m *observeMove) logf(format string, a ...any) {
	if m.r != nil && m.r.Log != nil {
		_, _ = fmt.Fprintf(m.r.Log, format+"\n", a...)
	}
}

func (m *observeMove) step(name string) {
	if f := m.o.observe.moveStep; f != nil {
		f(name)
	}
}

func (m *observeMove) run(ctx context.Context) error {
	o, p := m.o, m.p
	// The whole run: a settings sync waits, and observeReach stays quiet, so
	// there is no banner while observe is down for the copy.
	o.observe.mu.Lock()
	defer o.observe.mu.Unlock()
	if err := o.observeMoveLoad(ctx, p.NodeID, p.DiskID, p.Back); err != nil {
		return err
	}
	nctx := docker.WithNode(ctx, p.NodeID)
	srcName, dstName := installspec.ObserveVolume, installspec.ObserveDiskVolume
	if p.Back {
		srcName, dstName = dstName, srcName
	}
	src, dst := observeVolumeRow(srcName, p.NodeID), observeVolumeRow(dstName, p.NodeID)
	d, _, err := o.nodeDisk(ctx, p.NodeID)
	if err != nil {
		return err
	}

	m.logf("stopping stackr-observe")
	if err := o.upgrade.RemoveObserve(nctx); err != nil {
		return err
	}
	m.step("stopped")

	// Judge again with observe stopped: the size, the cap and the room are now
	// the ones the copy meets.
	size := o.volumes.SizeBytesFresh(nctx, src)
	if size < 0 {
		return m.fail(nctx, errs.Refusedf("Cannot read the size of the logs and metrics."))
	}
	free, err := o.observeDestFree(nctx, p.NodeID, p.Back)
	if err != nil {
		return m.fail(nctx, err)
	}
	if need := uint64(size + size/10); need > free {
		return m.fail(nctx, errs.Conflictf("Only %s is free and the copy needs %s (the data and a tenth over).", gb(free), gb(need)))
	} else if k, err := o.observeKnobs(ctx); err != nil {
		return m.fail(nctx, err)
	} else if !p.Back && free-need < k.minFree {
		return m.fail(nctx, errs.Conflictf("The disk would have %s free after the move, under the %s that logs and metrics keep free.", gb(free-need), gb(k.minFree)))
	}

	// A retry starts clean: the destination goes first, every time.
	m.logf("copying %s to %s", srcName, dstName)
	if err := o.volumes.RemoveNamed(nctx, p.NodeID, dstName); err != nil {
		return m.fail(nctx, err)
	}
	if !p.Back {
		c, err := o.observeCap(ctx, size)
		if err != nil {
			return m.fail(nctx, err)
		}
		if err := o.store.Settings.Set(ctx, observeDataCapKey, strconv.FormatUint(c, 10)); err != nil {
			return m.fail(nctx, err)
		}
	}
	if p.Back {
		// a plain volume made directly: EnsureNamed would place it on the node disk
		if err := o.docker.CreateVolume(nctx, dstName, "local", nil, nil); err != nil {
			return m.fail(nctx, err)
		}
		has, err := o.diskLeaf.HasFolders(ctx, d, dstName)
		if err != nil {
			return m.fail(nctx, err)
		}
		if found, dev, err := o.docker.VolumeDevice(nctx, dstName); err != nil {
			return m.fail(nctx, err)
		} else if !found || dev != "" || has[dstName] {
			return m.fail(nctx, errs.Conflictf("The plain volume came up on the disk."))
		}
	} else if err := o.volumes.EnsureNamed(nctx, p.NodeID, dstName, nil); err != nil {
		return m.fail(nctx, err)
	}
	if !p.Back {
		has, err := o.diskLeaf.HasFolders(ctx, d, dstName)
		if err != nil {
			return m.fail(nctx, err)
		}
		if found, dev, err := o.docker.VolumeDevice(nctx, dstName); err != nil {
			return m.fail(nctx, err)
		} else if !found || dev == "" || !has[dstName] {
			return m.fail(nctx, errs.Conflictf("The observe folder did not come up on the disk."))
		}
	}
	m.step("ensure")
	if err := o.volumes.EnsureTool(nctx, src); err != nil {
		return m.fail(nctx, err)
	}
	if err := pipeVolume(nctx, o, src, dst); err != nil {
		return m.fail(nctx, err)
	}
	m.step("copy")
	if !p.Back && size > 0 {
		// the copy must be proven present: ask the disk, fail on any error
		use, err := o.diskLeaf.Usage(ctx, d, true, dstName)
		if err != nil {
			return m.fail(nctx, fmt.Errorf("checking the copy: %w", err))
		}
		if u := use[dstName]; u.Missing || u.Empty {
			return m.fail(nctx, errs.Conflictf("The copy is missing or empty."))
		}
	}
	m.step("check")

	// Switch: save the setting, run observe on the new volume, prove it. Any
	// failure puts the old setting back and observe on its old volume.
	old, hadOld, err := o.store.Settings.Get(ctx, observeDataDiskKey)
	if err != nil {
		return m.fail(nctx, err)
	}
	if err := m.setOnDisk(ctx, !p.Back); err != nil {
		return m.fail(nctx, err)
	}
	o.syncObserveLocked(nctx)
	err = nil
	if e := o.observe.lastErr.Load(); e != nil && *e != "" {
		err = errors.New(*e)
	} else if o.ObserveOn() {
		err = o.observeCheck(nctx)
	}
	if err != nil {
		// The job ctx may be the reason; the rollback must not be cancelled with it.
		rb := context.WithoutCancel(ctx)
		var rerr error
		if hadOld {
			rerr = o.store.Settings.Set(rb, observeDataDiskKey, old)
		} else {
			rerr = o.store.Settings.Delete(rb, observeDataDiskKey)
		}
		if rerr == nil {
			o.syncObserveLocked(docker.WithNode(rb, p.NodeID))
			if e := o.observe.lastErr.Load(); e != nil && *e != "" {
				rerr = errors.New(*e)
			}
		}
		if rerr == nil && !p.Back {
			// the disk-side copy only; the original volume is never touched
			if cerr := o.volumes.RemoveNamed(docker.WithNode(rb, p.NodeID), p.NodeID, dstName); cerr != nil {
				m.logf("the failed copy %s was not removed: %v", dstName, cerr)
			}
		}
		if rerr != nil {
			return fmt.Errorf("observe did not come up on the new volume and the rollback is incomplete (check Settings, Observability): %w", errors.Join(err, rerr))
		}
		return fmt.Errorf("observe did not come up on the new volume, it is back on the old one: %w", err)
	}
	m.step("switched")

	// Only now the old volume goes (the disk folder and its row, on the way back).
	if err := o.volumes.RemoveNamed(nctx, p.NodeID, srcName); err != nil {
		return fmt.Errorf("moved, but the old copy was not removed: %w", err)
	}
	if p.Back {
		_ = o.store.Settings.Delete(ctx, observeDataCapKey)
	}
	action := auditObserveToDisk
	if p.Back {
		action = auditObserveFromDisk
	}
	if err := securitylog.New(o.store.SecurityEvents).Append(ctx, securitylog.Event{
		Action: action, ActorID: p.By, KeyID: p.KeyID, TargetKind: "node", TargetID: p.NodeID,
	}); err != nil {
		slog.Error("audit", "action", action, "error", err.Error())
	}
	m.logf("logs and metrics are on %s", dstName)
	m.step("done")
	return nil
}

func (m *observeMove) setOnDisk(ctx context.Context, on bool) error {
	if on {
		return m.o.store.Settings.Set(ctx, observeDataDiskKey, "1")
	}
	return m.o.store.Settings.Delete(ctx, observeDataDiskKey)
}

// fail ends a run before the switch: observe is started again on the volume it
// had (the setting never changed), and the cause is returned.
func (m *observeMove) fail(ctx context.Context, cause error) error {
	m.o.syncObserveLocked(context.WithoutCancel(ctx))
	if !m.p.Back {
		// the half copy on the disk goes; the original volume is never touched
		if err := m.o.volumes.RemoveNamed(context.WithoutCancel(ctx), m.p.NodeID, installspec.ObserveDiskVolume); err != nil {
			m.logf("the failed copy %s was not removed: %v", installspec.ObserveDiskVolume, err)
		}
	}
	if e := m.o.observe.lastErr.Load(); e != nil && *e != "" {
		return errors.Join(cause, fmt.Errorf("the rollback is incomplete, observe is not back on the old volume (check Settings, Observability): %s", *e))
	}
	return cause
}

// observeAnswers is the proof after a move: both sockets answer and a test
// point is written. A write is not read back at once: VictoriaMetrics shows a
// new point only after its search latency.
func (o *Orchestrator) observeAnswers(ctx context.Context) error {
	if o.upgrade.ObserveOff() {
		return nil // nothing runs on this box
	}
	var err error
	for range 60 { // the container needs a moment to open its sockets
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if _, err = o.victoria().DataSize(cctx); err == nil && o.logship.on.Load() {
			_, err = o.victoriaLogs().DataSize(cctx)
		}
		if err == nil {
			err = o.victoria().Write(cctx, []metrics.Point{{Key: "node:observe-move", Name: "check", T: time.Now(), V: 1}})
		}
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return err
}

// observeCheck is the proof, or the test's stand-in for it.
func (o *Orchestrator) observeCheck(ctx context.Context) error {
	if o.observe.check != nil {
		return o.observe.check(ctx)
	}
	return o.observeAnswers(ctx)
}
