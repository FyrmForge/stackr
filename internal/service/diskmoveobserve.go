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

// observeOnDisk is whether observe's data volume is the disk folder.
func (o *Orchestrator) observeOnDisk(ctx context.Context) bool {
	v, ok, err := o.store.Settings.Get(ctx, observeDataDiskKey)
	return err == nil && ok && v == "1"
}

// observeDiskRefusal is the refusal Forget (and Adopt) of a node's disk gives
// while observe's data is on it: move it back first. nil when it is not.
func (o *Orchestrator) observeDiskRefusal(ctx context.Context, nodeID string) error {
	if !o.observeOnDisk(ctx) {
		return nil
	}
	if ctl, err := o.controllerID(ctx); err != nil || ctl != nodeID {
		return err
	}
	return errs.Refusedf("Logs and metrics are on this disk. Move them back to the system disk first.")
}

// observeDiskReady is whether the controller has a ready node disk.
func (o *Orchestrator) observeDiskReady(ctx context.Context) bool {
	ctl, err := o.controllerID(ctx)
	if err != nil {
		return false
	}
	d, has, err := o.nodeDisk(ctx, ctl)
	return err == nil && has && d.State == disk.StateReady
}

// observeCap is the xfs cap of the disk folder: twice today's data, and never
// under what the logs may grow to (their own size cap) plus a gigabyte for
// metrics, or the cap would stop observe before its own limits do.
// ponytail: the floor is read once at the move; editable later.
func (o *Orchestrator) observeCap(ctx context.Context, size int64) uint64 {
	c := uint64(max(size, 0)) * 2
	if k, err := o.observeKnobs(ctx); err == nil && k.logs {
		c = max(c, k.logsMax+1<<30)
	}
	return max(c, 1<<30)
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
	if err := o.observeMoveLoad(ctx, nodeID, back); err != nil {
		return p, err
	}
	src := installspec.ObserveVolume
	if back {
		src = installspec.ObserveDiskVolume
	}
	p.SizeBytes = o.volumes.SizeBytesFresh(ctx, observeVolumeRow(src, nodeID))
	var err error
	if back {
		p.Free, _ = o.diskOf(ctx, nodeID)
	} else {
		p.Cap = o.observeCap(ctx, p.SizeBytes)
		p.Free, _, err = o.dataDiskOf(ctx, nodeID)
		if err != nil {
			return p, err
		}
	}
	n := max(p.SizeBytes, 0)
	p.Need = n + n/10
	p.Fits = uint64(p.Need) <= p.Free
	if p.Busy, err = o.todiskLive(ctx, nodeID); err != nil {
		return p, err
	}
	return p, nil
}

// observeMoveLoad is every refusal of the move that does not depend on size.
func (o *Orchestrator) observeMoveLoad(ctx context.Context, nodeID string, back bool) error {
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
	if err := o.erroringDiskGate(ctx, nodeID); err != nil {
		return err
	}
	if on := o.observeOnDisk(ctx); on != back {
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
	uid, kid := actorOf(by)
	return o.enqueueExclusive(ctx, kindObserveMove, observeMoveJob{NodeID: nodeID, Back: back, By: uid, KeyID: kid},
		[]string{"observe-move"}, errs.Conflictf("Logs and metrics are already moving."), todiskKey(nodeID))
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
	if err := o.observeMoveLoad(ctx, p.NodeID, p.Back); err != nil {
		return err
	}
	nctx := docker.WithNode(ctx, p.NodeID)
	srcName, dstName := installspec.ObserveVolume, installspec.ObserveDiskVolume
	if p.Back {
		srcName, dstName = dstName, srcName
	}
	src, dst := observeVolumeRow(srcName, p.NodeID), observeVolumeRow(dstName, p.NodeID)
	size := max(o.volumes.SizeBytesFresh(nctx, src), 0)
	d, _, err := o.nodeDisk(ctx, p.NodeID)
	if err != nil {
		return err
	}

	m.logf("stopping stackr-observe")
	if err := o.upgrade.RemoveObserve(nctx); err != nil {
		return err
	}
	m.step("stopped")

	// A retry starts clean: the destination goes first, every time.
	m.logf("copying %s to %s", srcName, dstName)
	if err := o.volumes.RemoveNamed(nctx, p.NodeID, dstName); err != nil {
		return m.fail(nctx, err)
	}
	if !p.Back {
		if err := o.store.Settings.Set(ctx, observeDataCapKey, strconv.FormatUint(o.observeCap(ctx, size), 10)); err != nil {
			return m.fail(nctx, err)
		}
	}
	if err := o.volumes.EnsureNamed(nctx, p.NodeID, dstName, nil); err != nil {
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
	old, hadOld, _ := o.store.Settings.Get(ctx, observeDataDiskKey)
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
		if hadOld {
			_ = o.store.Settings.Set(ctx, observeDataDiskKey, old)
		} else {
			_ = o.store.Settings.Delete(ctx, observeDataDiskKey)
		}
		o.syncObserveLocked(nctx)
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
