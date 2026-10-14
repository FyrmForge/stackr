package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	lrun "github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The gates: work for a node that is offline is refused before a row is
// written, a worker too far behind the panel gets no new Docker calls, and a
// deploy checks the node's disk. The decisions live here; the router and the
// verbs only ask.

const (
	// minFreeDisk is what a deploy needs on the node's Docker disk.
	minFreeDisk = 1 << 30
	// lowDiskPct is the free share under which a deploy warns.
	lowDiskPct = 10
	// prevVersionKey is the panel version before the last upgrade (written at boot).
	prevVersionKey = "panel_prev_version"
)

// seenKey is the setting that keeps when a node was last seen (unix seconds),
// written when it flips to offline, so a panel restart does not forget it.
func seenKey(nodeID string) string { return "node.seen." + nodeID }

// NodeDownError is a refusal because a worker is not online. It is an
// errs.ErrBusy (503) that says which node and since when.
type NodeDownError struct {
	Name     string
	LastSeen time.Time // zero = never seen
}

func (e *NodeDownError) Error() string {
	if e.LastSeen.IsZero() {
		return e.Name + " is offline, never seen"
	}
	return e.Name + " is offline, last seen " + ago(time.Since(e.LastSeen))
}

func (e *NodeDownError) Unwrap() error { return errs.ErrBusy }

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
}

// NodeDown is what the env and tile pages show when their node is offline.
type NodeDown struct {
	Name     string
	LastSeen time.Time
}

type allowOldKey struct{}

// allowOld marks ctx so router calls skip the too-old refusal: the worker
// upgrade itself has to reach a node that is too old to deploy to.
func allowOld(ctx context.Context) context.Context {
	return context.WithValue(ctx, allowOldKey{}, true)
}

func oldAllowed(ctx context.Context) bool {
	v, _ := ctx.Value(allowOldKey{}).(bool)
	return v
}

// lastSeen is when the controller last heard from a node: the mesh's copy,
// else what was kept when the node flipped offline. Zero = never.
func (o *Orchestrator) lastSeen(id string) time.Time {
	if o.mesh != nil {
		if t := o.mesh.seenAt(id); !t.IsZero() {
			return t
		}
	}
	v, ok, err := o.store.Settings.Get(context.Background(), seenKey(id))
	if err != nil || !ok {
		return time.Time{}
	}
	if s, err := strconv.ParseInt(v, 10, 64); err == nil && s > 0 {
		return time.Unix(s, 0)
	}
	return time.Time{}
}

// markSeen keeps a node's last-seen time as it goes offline.
func (o *Orchestrator) markSeen(ctx context.Context, id string) {
	t := o.lastSeen(id)
	if t.IsZero() {
		t = time.Now()
	}
	if err := o.store.Settings.Set(ctx, seenKey(id), strconv.FormatInt(t.Unix(), 10)); err != nil {
		slog.Warn("nodes: last seen not kept", "node", id, "err", err)
	}
}

// nodeDown is the refusal for a node by id.
func (o *Orchestrator) nodeDown(ctx context.Context, id string) *NodeDownError {
	name := id
	if n, err := o.store.Nodes.Get(ctx, id); err == nil {
		name = n.Name
	}
	return &NodeDownError{Name: name, LastSeen: o.lastSeen(id)}
}

// nodeUp refuses a worker that is not online. The controller is always up,
// and a node that does not exist is for the caller's own lookup to report.
func (o *Orchestrator) nodeUp(ctx context.Context, nodeID string) error {
	n, err := o.store.Nodes.Get(ctx, nodeID)
	if errors.Is(err, errs.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if n.Role == node.Worker && n.Status != node.StatusOnline {
		return o.nodeDown(ctx, nodeID)
	}
	return nil
}

// EnvNodeDown reports the env's node when it is an offline worker.
func (o *Orchestrator) EnvNodeDown(ctx context.Context, envID string) (NodeDown, bool) {
	e, err := o.store.Environments.Get(ctx, envID)
	if err != nil || e.NodeID == "" {
		return NodeDown{}, false
	}
	var d *NodeDownError
	if errors.As(o.nodeUp(ctx, e.NodeID), &d) {
		return NodeDown{Name: d.Name, LastSeen: d.LastSeen}, true
	}
	return NodeDown{}, false
}

// gateEnv is what a verb on an env's node asks before it writes a row: the
// node is up and, for a deploy, has the disk. An env that cannot be read is
// left to the verb's own lookup to report.
func (o *Orchestrator) gateEnv(ctx context.Context, envID string, disk bool) error {
	e, err := o.store.Environments.Get(ctx, envID)
	if err != nil || e.NodeID == "" {
		return nil
	}
	if err := o.nodeUp(ctx, e.NodeID); err != nil {
		return err
	}
	if disk {
		_, err = o.diskCheck(ctx, e.NodeID)
	}
	return err
}

// gateTile is gateEnv for a tile's env.
func (o *Orchestrator) gateTile(ctx context.Context, tileID string, disk bool) error {
	t, err := o.store.Tiles.Get(ctx, tileID)
	if err != nil {
		return nil
	}
	return o.gateEnv(ctx, t.EnvironmentID, disk)
}

// diskOf is the free and total bytes of a node's Docker disk as it reported
// them; 0, 0 when it did not. The controller falls back to its data dir.
func (o *Orchestrator) diskOf(ctx context.Context, nodeID string) (free, total uint64) {
	n, err := o.store.Nodes.Get(ctx, nodeID)
	if err != nil {
		return 0, 0
	}
	if f := o.reportedFacts(ctx, n); f != nil {
		free, total = f.DiskFree, f.DiskTotal
	}
	if n.Role == node.Controller && free == 0 && total == 0 {
		free, total = o.dataDirDisk()
	}
	return free, total
}

// dataDirDisk is the free and total bytes of the disk under the data dir.
func (o *Orchestrator) dataDirDisk() (free, total uint64) {
	var st syscall.Statfs_t
	if syscall.Statfs(o.cfg.DataDir, &st) != nil {
		return 0, 0
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize)
}

func gb(b uint64) string { return fmt.Sprintf("%.1f GB", float64(b)/(1<<30)) }

// diskCheck judges a node's Docker disk for a deploy: a refusal under 1 GB
// free, a warning under 10%, a note when the node does not report its disk.
// Never a refusal for a disk it cannot read.
func (o *Orchestrator) diskCheck(ctx context.Context, nodeID string) (warn string, err error) {
	free, total := o.diskOf(ctx, nodeID)
	name := nodeID
	if n, err := o.store.Nodes.Get(ctx, nodeID); err == nil {
		name = n.Name
	}
	switch {
	case free == 0 && total == 0:
		return "disk not checked: " + name + " does not report its disk", nil
	case free < minFreeDisk:
		return "", errs.Conflictf("%s has %s free on its Docker disk; deploys need 1 GB. Free space and retry.", name, gb(free))
	case total > 0 && free*100 < total*lowDiskPct:
		return fmt.Sprintf("warning: %s has %d%% (%s) free on its Docker disk", name, free*100/total, gb(free)), nil
	}
	return "", nil
}

// preflight is the deploy's disk step: it logs the warning and returns the
// refusal; it is deploy.Flow.Preflight.
func (o *Orchestrator) preflight(ctx context.Context, e store.Environment, log io.Writer) error {
	id := e.NodeID
	if id == "" {
		var err error
		if id, err = o.controllerID(ctx); err != nil {
			return err
		}
	}
	warn, err := o.diskCheck(ctx, id)
	if warn != "" {
		if !strings.HasPrefix(warn, "warning: ") {
			warn = "warning: " + warn
		}
		_, _ = fmt.Fprintln(log, warn)
	}
	return err
}

// versionAllowed: a worker may run the panel's version or the one the panel
// ran before its last upgrade. Anything else, an empty version included, is
// too old.
func (o *Orchestrator) versionAllowed(ctx context.Context, v string) bool {
	if v == "" {
		return false
	}
	if v == o.cfg.Version {
		return true
	}
	prev, _, _ := o.store.Settings.Get(ctx, prevVersionKey)
	return prev != "" && v == prev
}

// gate is the router's check on every Docker call: a node with no connection
// is down; a connected one that is too old is refused unless ctx allows old.
func (o *Orchestrator) gate(ctx context.Context, nodeID string, connected bool) error {
	if !connected {
		return o.nodeDown(ctx, nodeID)
	}
	if o.cluster == nil || oldAllowed(ctx) {
		return nil
	}
	v, bad := o.cluster.tooOldVersion(nodeID)
	if !bad {
		return nil
	}
	name := nodeID
	if n, err := o.store.Nodes.Get(ctx, nodeID); err == nil {
		name = n.Name
	}
	if v == "" {
		v = "(unknown version)"
	}
	return errs.Conflictf("%s runs stackr %s, too old for this panel (%s); upgrade it from the Update tab.", name, v, o.cfg.Version)
}

// skip records a job that never ran because its node is offline.
func (o *Orchestrator) skip(ctx context.Context, kind jobs.Kind, p any, reason string, lock ...string) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = o.jobs.Skip(ctx, kind, lock, string(o.withOrg(ctx, b)), reason)
	return err
}

// skipIfDown records the schedule as skipped and reports true when the node
// is offline.
func (o *Orchestrator) skipIfDown(ctx context.Context, err error, kind jobs.Kind, p any, lock ...string) (bool, error) {
	var d *NodeDownError
	if !errors.As(err, &d) {
		return false, err
	}
	return true, o.skip(ctx, kind, p, "skipped: "+d.Name+" offline", lock...)
}

// scheduleBackup is the schedule driver for a volume backup: queued, or
// skipped when the volume's node is offline.
func (o *Orchestrator) scheduleBackup(ctx context.Context, s store.BackupSchedule) error {
	p, lock := backupJob{ScheduleID: s.ID, VolumeID: s.VolumeID}, "volume:"+s.VolumeID
	err := o.volumeNodeUp(ctx, s.VolumeID)
	if skipped, err := o.skipIfDown(ctx, err, kindBackup, p, lock); skipped || err != nil {
		return err
	}
	_, err = o.enqueue(ctx, kindBackup, p, lock)
	return err
}

// volumeNodeUp is nodeUp for the node a volume's data lives on.
func (o *Orchestrator) volumeNodeUp(ctx context.Context, volumeID string) error {
	v, err := o.store.Volumes.Get(ctx, volumeID)
	if err != nil {
		return nil // the backup job reports a missing volume itself
	}
	id := v.NodeID
	if id == "" {
		if id, err = o.scopeNode(ctx, v.ScopeKind, v.ScopeID); err != nil {
			return nil
		}
	}
	return o.nodeUp(ctx, id)
}

// scheduleCron is the schedule driver for a cron tile: a run is queued, or
// skipped when the tile's node is offline.
func (o *Orchestrator) scheduleCron(ctx context.Context, t store.Tile) error {
	err := o.gateEnv(ctx, t.EnvironmentID, false)
	if skipped, err := o.skipIfDown(ctx, err, kindRun, runJob{TileID: t.ID}, t.ID); skipped || err != nil {
		return err
	}
	_, _, err = o.queueRun(ctx, t.ID, lrun.Schedule)
	return err
}
