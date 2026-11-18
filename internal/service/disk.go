package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A node's disk: one folder on the node, a mounted path that carries a stamp,
// where its volumes live. "No disk" is no node_disks row and nothing else;
// any other state refuses (leaf/disk).

const (
	kindDiskAdd       jobs.Kind = "disk-add"
	kindDiskCheck     jobs.Kind = "disk-check"
	kindDiskAddDevice jobs.Kind = "disk-add-device"
	kindDiskGrow      jobs.Kind = "disk-grow"

	auditDiskAdd       = "disk.add"
	auditDiskCheck     = "disk.check"
	auditDiskAddDevice = "disk.add_device"
	auditDiskGrow      = "disk.grow"
)

// NodeDiskView is a node's disk as the panel shows it.
type NodeDiskView struct {
	Has         bool
	ID          string // the disk row's id
	Kind, Path  string
	State       string // adding | ready | missing
	Free, Total uint64
	Network     bool
	ReadAt      time.Time // when the worker last read the disk; zero = never
	Stale       bool      // the worker's last read of the disk is old, or there is none
	Volumes     int
	// xfs: the device behind the disk. NewSpace is the bytes the device has
	// beyond what the disk uses ("Use it" grows the disk into them); Damaged
	// is "smaller" (the device shrank) or "other_fs" (another filesystem sits
	// on it), and the disk is not to be trusted until that is looked at.
	// Both come from the worker's last facts and stay empty without them.
	Device      string
	DeviceBytes uint64
	NewSpace    uint64
	Damaged     string
	// WasBytes is the device's size when the disk was added or last grown;
	// Formatted is whether the add already made the filesystem (a retry then
	// only mounts it).
	WasBytes  uint64
	Formatted bool
}

// FolderProbe is what a path on a node looks like before it is added. A
// refusal (not a mount point, the root filesystem, not empty ...) is an
// answer: Code and Message say it, and the error is nil.
type FolderProbe struct {
	Path                            string
	FSType                          string
	Mounted, RootFS, Network, Empty bool
	Free, Total                     uint64
	StampOwner                      string // the node a stamp on it names, "" = none
	Code, Message                   string
}

type diskJob struct {
	NodeID string `json:"node_id"`
	Path   string `json:"path,omitempty"`
	// add device: the disk as the person picked it, and whether they chose
	// Format anyway.
	Device *hostdisk.Device `json:"device,omitempty"`
	Force  bool             `json:"force,omitempty"`
	By     string           `json:"by,omitempty"`
	KeyID  string           `json:"key_id,omitempty"`
}

// nodeDisk is the node's disk row. A lookup error is a failure everywhere,
// never "no disk".
func (o *Orchestrator) nodeDisk(ctx context.Context, nodeID string) (store.NodeDisk, bool, error) {
	if nodeID == "" {
		return store.NodeDisk{}, false, nil
	}
	return o.diskLeaf.OfNode(ctx, nodeID)
}

// diskAdapter is leaf/disk as the volume leaf's Disk. It turns leaf/disk's
// *MissingError into the volume leaf's own, which that leaf checks for.
type diskAdapter struct{ o *Orchestrator }

func missing(err error) error {
	var m *disk.MissingError
	if errors.As(err, &m) {
		return &volume.MissingError{Node: m.Node}
	}
	return err
}

func (a diskAdapter) State(ctx context.Context, nodeID string) (volume.DiskState, error) {
	d, ok, err := a.o.nodeDisk(ctx, nodeID)
	switch {
	case err != nil:
		return volume.DiskNone, err // never read as "no disk"
	case !ok:
		return volume.DiskNone, nil
	case d.State == disk.StateReady, opMode(ctx, d) == disk.StateAdopting:
		return volume.DiskReady, nil
	case opMode(ctx, d) == disk.StateForgetting:
		return volume.DiskNone, nil // only the forget job sees plain volumes here
	}
	return volume.DiskNotReady, nil
}

func (a diskAdapter) Folders(ctx context.Context, nodeID string, names ...string) (map[string]bool, error) {
	d, ok, err := a.o.nodeDisk(ctx, nodeID)
	if err != nil || !ok {
		return map[string]bool{}, err
	}
	return a.o.diskLeaf.HasFolders(ctx, d, names...)
}

// ready is the row of a node whose disk is there to work on. No row is
// *MissingError too: the caller asked after a state that has since gone.
func (a diskAdapter) ready(ctx context.Context, nodeID string) (store.NodeDisk, error) {
	d, ok, err := a.o.nodeDisk(ctx, nodeID)
	if err != nil {
		return d, err
	}
	if !ok || d.State != disk.StateReady && opMode(ctx, d) != disk.StateAdopting {
		return d, &volume.MissingError{Node: nodeID}
	}
	return d, nil
}

// Path is the folder a volume is bound to, from the row alone: no host call.
func (a diskAdapter) Path(ctx context.Context, nodeID, name string) (string, error) {
	d, err := a.ready(ctx, nodeID)
	if err != nil {
		return "", err
	}
	return docker.DeviceOf(disk.BindOpts(d.MountPath, d.ID, name)), nil
}

func (a diskAdapter) Place(ctx context.Context, nodeID, name string) (map[string]string, error) {
	d, err := a.ready(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if a.o.diskDamaged(ctx, d) { // every deploy places its folder: this stops them all
		return nil, &volume.MissingError{Node: nodeID, Msg: "The disk's device changed. Check the disk."}
	}
	opts, err := a.o.diskLeaf.Place(ctx, d, name)
	return opts, missing(err)
}

func (a diskAdapter) Usage(ctx context.Context, nodeID string, mode volume.UseMode, names ...string) (map[string]volume.Use, error) {
	d, err := a.ready(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	var use map[string]hostdisk.Use
	switch mode {
	case volume.UseCached:
		use = a.o.cachedSizes(d, names)
	default:
		if use, err = a.o.diskLeaf.Usage(ctx, d, mode == volume.UseFresh, names...); err != nil {
			return nil, missing(err)
		}
	}
	out := make(map[string]volume.Use, len(use))
	for n, u := range use {
		out[n] = volume.Use{Bytes: u.Bytes, Empty: u.Empty, Missing: u.Missing}
	}
	return out, nil
}

func (a diskAdapter) Drop(ctx context.Context, nodeID, name string) error {
	d, err := a.ready(ctx, nodeID)
	if err != nil {
		return err
	}
	return missing(a.o.diskLeaf.Drop(ctx, d, name))
}

// NodeDisk is the node's disk for its drawer. Free and total come from the
// worker's last facts, never from a helper run.
func (o *Orchestrator) NodeDisk(ctx context.Context, nodeID string) (NodeDiskView, error) {
	d, ok, err := o.nodeDisk(ctx, nodeID)
	if err != nil || !ok {
		return NodeDiskView{}, err
	}
	fs, err := o.diskLeaf.Folders(ctx, d)
	if err != nil {
		return NodeDiskView{}, err
	}
	v := NodeDiskView{Has: true, ID: d.ID, Kind: d.Kind, Path: d.MountPath, State: d.State, Volumes: len(fs), Stale: true, Device: d.DeviceID,
		WasBytes: uint64(max(d.DeviceBytes, 0)), Formatted: d.FSUUID != ""}
	if d.State == disk.StateAdopting && d.NodeID != nodeID { // before the commit the row is still the old node's
		v.Path = d.OpMountPath
	}
	n, err := o.store.Nodes.Get(ctx, nodeID)
	if err != nil {
		return v, err
	}
	if f := o.diskFacts(ctx, n); f != nil && f.DisksRead {
		if i := slices.IndexFunc(f.Disks, func(x hostdisk.Fact) bool { return x.Path == d.MountPath }); i >= 0 {
			x := f.Disks[i]
			v.Free, v.Total, v.Network, v.Stale, v.ReadAt = x.Free, x.Total, x.Network, f.DisksStale, x.ReadAt
			v.DeviceBytes, v.NewSpace, v.Damaged = deviceFacts(d, x, o.diskLeaf.GrownAt(d.ID))
		}
	}
	// A controller with no worker has no facts. When the panel sees the mount
	// itself (not an installed panel) and it is still a mount, one statfs is
	// the same answer. An unmounted path would show the root filesystem.
	if v.Total == 0 && d.State == disk.StateReady && o.dh.inProcess(ctx, nodeID) {
		if free, total, ok := mountStatfs(d.MountPath); ok {
			v.Free, v.Total = free, total
			v.Stale, v.ReadAt = false, time.Now()
		}
	}
	return v, nil
}

// mountStatfs is the free and total bytes of p when p is a mount point. It
// gives up after a second so a dead network mount cannot hold a poll.
// ponytail: a hung call leaks one goroutine until the mount answers.
func mountStatfs(p string) (free, total uint64, ok bool) {
	type res struct {
		free, total uint64
		ok          bool
	}
	ch := make(chan res, 1)
	go func() {
		dev, err := hostdisk.DevString(p)
		parent, err2 := hostdisk.DevString(filepath.Dir(p))
		var st syscall.Statfs_t
		if err != nil || err2 != nil || dev == parent || syscall.Statfs(p, &st) != nil || st.Blocks == 0 {
			ch <- res{}
			return
		}
		ch <- res{st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), true}
	}()
	select {
	case r := <-ch:
		return r.free, r.total, r.ok
	case <-time.After(time.Second):
		return 0, 0, false
	}
}

// diskNodeUp is the node a disk verb may work on: an online one.
func (o *Orchestrator) diskNodeUp(ctx context.Context, nodeID string) (store.Node, error) {
	n, err := o.nodeLeaf().Get(ctx, nodeID)
	if err != nil {
		return n, err
	}
	if n.Status == node.StatusPending {
		return n, errs.ErrNotFound
	}
	return n, o.nodeUp(ctx, n.ID)
}

func (o *Orchestrator) auditDiskLog(ctx context.Context, by *Principal, action, nodeID, detail string) {
	uid, kid := actorOf(by)
	if err := securitylog.New(o.store.SecurityEvents).Append(ctx, securitylog.Event{
		Action: action, ActorID: uid, KeyID: kid, TargetKind: "node", TargetID: nodeID, Detail: detail,
	}); err != nil {
		slog.Error("audit", "action", action, "error", err.Error())
	}
}

// probeRefusals are the answers a probe shows the person; any other failure
// (an I/O error) is an error, not a verdict on the folder.
var probeRefusals = []string{hostdisk.CodeNotMountPoint, hostdisk.CodeRootFS, hostdisk.CodeNotEmpty,
	hostdisk.CodeForeignStamp, hostdisk.CodeBadName, hostdisk.CodeUnsupported, hostdisk.CodeDiskMissing, hostdisk.CodeReadOnly}

// ProbeNodeFolder reads a path on the node and writes nothing.
func (o *Orchestrator) ProbeNodeFolder(ctx context.Context, by *Principal, nodeID, path string) (FolderProbe, error) {
	if err := nodeAdmin(by); err != nil {
		return FolderProbe{}, err
	}
	if err := diskPath(path); err != nil {
		return FolderProbe{}, err
	}
	n, err := o.diskNodeUp(ctx, nodeID)
	if err != nil {
		return FolderProbe{}, err
	}
	if err := o.folderDiskOK(ctx, n.ID); err != nil {
		return FolderProbe{}, err
	}
	f, err := o.diskLeaf.Probe(ctx, nodeID, path)
	var he *hostdisk.Error
	if errors.As(err, &he) && slices.Contains(probeRefusals, he.Code) {
		return FolderProbe{Path: path, Code: he.Code, Message: he.Message}, nil
	}
	if err != nil {
		return FolderProbe{}, err
	}
	return FolderProbe{
		Path: path, FSType: f.FSType, Mounted: f.Mounted, RootFS: f.RootFS, Network: f.Network, Empty: f.Empty,
		Free: f.Free, Total: f.Total, StampOwner: f.StampOwner,
	}, nil
}

// folderDiskOK refuses the controller of an installed panel: its worker runs
// in the panel's container and cannot see the host's folders.
func (o *Orchestrator) folderDiskOK(ctx context.Context, nodeID string) error {
	if !o.cfg.installedPanel() {
		return nil
	}
	ctl, err := o.controllerID(ctx)
	if err != nil {
		return err
	}
	if ctl == nodeID {
		return errs.Refusedf("The panel cannot use a folder disk yet. Add the disk on a worker node.")
	}
	return nil
}

func diskPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return errs.Invalidf("path", "Give a full path to a mounted folder, like /mnt/data.")
	}
	return nil
}

// AddNodeFolder queues making the mounted folder at path the node's disk.
func (o *Orchestrator) AddNodeFolder(ctx context.Context, by *Principal, nodeID, path string) (Job, error) {
	if err := nodeAdmin(by); err != nil {
		return Job{}, err
	}
	if err := diskPath(path); err != nil {
		return Job{}, err
	}
	n, err := o.diskNodeUp(ctx, nodeID)
	if err != nil {
		return Job{}, err
	}
	if err := o.folderDiskOK(ctx, n.ID); err != nil {
		return Job{}, err
	}
	d, has, err := o.nodeDisk(ctx, n.ID)
	if err != nil {
		return Job{}, err
	}
	if has && d.State != disk.StateAdding { // an adding row, at any path, is replaced by this add
		return Job{}, errs.Conflictf("This node already has a disk.")
	}
	// A worker too old to say what it holds would never be told which folder
	// is the disk, and so could not report it.
	if o.diskWorker(n) {
		if f := o.diskFacts(ctx, n); f == nil || !f.DisksRead {
			return Job{}, errs.Refusedf("This node's worker is too old to manage a disk. Update it first.")
		}
	}
	lock, err := o.nodeLock(ctx, n)
	if err != nil {
		return Job{}, err
	}
	j, err := o.enqueue(ctx, kindDiskAdd, diskJob{NodeID: n.ID, Path: path, By: by.User.ID, KeyID: by.KeyID}, lock...)
	if err == nil {
		o.auditDiskLog(ctx, by, auditDiskAdd, n.ID, path)
	}
	return j, err
}

// CheckNodeDisk queues the disk check: the host verifies the mount and the
// stamp, the row goes ready, and the tiles with disk volumes that are not
// running deploy. It is the only way out of missing.
func (o *Orchestrator) CheckNodeDisk(ctx context.Context, by *Principal, nodeID string) (Job, error) {
	if err := nodeAdmin(by); err != nil {
		return Job{}, err
	}
	n, err := o.diskNodeUp(ctx, nodeID)
	if err != nil {
		return Job{}, err
	}
	if _, has, err := o.nodeDisk(ctx, n.ID); err != nil {
		return Job{}, err
	} else if !has {
		return Job{}, errs.ErrNotFound
	}
	lock, err := o.nodeLock(ctx, n)
	if err != nil {
		return Job{}, err
	}
	j, err := o.enqueue(ctx, kindDiskCheck, diskJob{NodeID: n.ID, By: by.User.ID, KeyID: by.KeyID}, lock...)
	if err == nil {
		o.auditDiskLog(ctx, by, auditDiskCheck, n.ID, "")
	}
	return j, err
}

// DiskDevice is a whole block device on a node as the picker shows it: what
// it is, and why it can or cannot be used. The pick comes back whole into
// AddNodeDevice, so a device that changed since the list is caught.
type DiskDevice = hostdisk.Device

// DeviceBlockedWhy is the picker's reason a device cannot be used
// (DiskDevice.Blocked), in words.
func DeviceBlockedWhy(blocked string) string { return disk.BlockedWhy(blocked) }

// NodeDevices lists the node's whole disks, each with why it can or cannot be
// picked. It runs the host helper: a verb of the add dialog, never a poll.
func (o *Orchestrator) NodeDevices(ctx context.Context, by *Principal, nodeID string) ([]DiskDevice, error) {
	if err := nodeAdmin(by); err != nil {
		return nil, err
	}
	n, err := o.diskNodeUp(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	return o.diskLeaf.Devices(ctx, n.ID)
}

// AddNodeDevice queues making the picked device the node's disk: it is
// formatted xfs, mounted and stamped. Formatting destroys what is on it, so
// the person must type the node's name or the device (typed), and a disk that
// already holds a filesystem, a partition table or an earlier stackr disk is
// only taken with force ("Format anyway"). A device that cannot be formatted
// at all (mounted, partitions, system ...) is refused with no override.
func (o *Orchestrator) AddNodeDevice(ctx context.Context, by *Principal, nodeID string, pick DiskDevice, typed string, force bool) (Job, error) {
	if err := nodeAdmin(by); err != nil {
		return Job{}, err
	}
	n, err := o.diskNodeUp(ctx, nodeID)
	if err != nil {
		return Job{}, err
	}
	if pick.ID == "" {
		return Job{}, errs.Invalidf("device", "Pick a disk.")
	}
	// the person types the device (sdb, or its path) so the wrong disk is not wiped by a click
	if pick.Path == "" || typed != filepath.Base(pick.Path) && typed != pick.Path && typed != pick.ID {
		return Job{}, errs.Invalidf("confirm", "type %s to confirm", filepath.Base(pick.Path))
	}
	d, has, err := o.nodeDisk(ctx, n.ID)
	if err != nil {
		return Job{}, err
	}
	if has && d.State != disk.StateAdding {
		return Job{}, errs.Conflictf("This node already has a disk.")
	}
	// A retry of an add that stopped after the format: the device carries the
	// filesystem this very add made, so it is mounted, never formatted again.
	resume := has && d.Kind == disk.KindXFS && d.DeviceID == pick.ID && d.FSUUID != "" && d.FSUUID == pick.FSUUID
	if resume {
		force = false
	} else {
		if pick.Blocked != "" {
			return Job{}, errs.Refusedf("That disk cannot be used: %s.", disk.BlockedWhy(pick.Blocked))
		}
		if pick.Signature != "" && !force {
			return Job{}, errs.Refusedf(disk.MsgNeedsForce)
		}
		force = force && pick.Signature != ""
	}
	if o.diskWorker(n) {
		if f := o.diskFacts(ctx, n); f == nil || !f.DisksRead {
			return Job{}, errs.Refusedf("This node's worker is too old to manage a disk. Update it first.")
		}
	}
	lock, err := o.nodeLock(ctx, n)
	if err != nil {
		return Job{}, err
	}
	j, err := o.enqueue(ctx, kindDiskAddDevice, diskJob{NodeID: n.ID, Device: &pick, Force: force, By: by.User.ID, KeyID: by.KeyID}, lock...)
	if err == nil {
		o.auditDiskLog(ctx, by, auditDiskAddDevice, n.ID, pick.ID)
	}
	return j, err
}

// GrowNodeDisk queues growing an xfs disk's filesystem into the space its
// device gained ("Use it"). The disk stays in use meanwhile.
func (o *Orchestrator) GrowNodeDisk(ctx context.Context, by *Principal, nodeID string) (Job, error) {
	if err := nodeAdmin(by); err != nil {
		return Job{}, err
	}
	n, err := o.diskNodeUp(ctx, nodeID)
	if err != nil {
		return Job{}, err
	}
	d, has, err := o.nodeDisk(ctx, n.ID)
	switch {
	case err != nil:
		return Job{}, err
	case !has:
		return Job{}, errs.ErrNotFound
	case d.Kind != disk.KindXFS:
		return Job{}, errs.Refusedf("Only a disk made from a whole device can grow.")
	case d.State != disk.StateReady:
		return Job{}, errs.Refusedf("The disk is %s. Check it first.", d.State)
	}
	lock, err := o.nodeLock(ctx, n)
	if err != nil {
		return Job{}, err
	}
	j, err := o.enqueue(ctx, kindDiskGrow, diskJob{NodeID: n.ID, By: by.User.ID, KeyID: by.KeyID}, lock...)
	if err == nil {
		o.auditDiskLog(ctx, by, auditDiskGrow, n.ID, d.DeviceID)
	}
	return j, err
}

func (o *Orchestrator) runDiskAddDevice(ctx context.Context, r *jobs.Run, p diskJob) error {
	n, err := o.nodeLeaf().Get(ctx, p.NodeID)
	if err != nil {
		return err
	}
	if p.Device == nil {
		return errors.New("disk: add device without a device")
	}
	_, _ = fmt.Fprintf(r.Log, "adding %s on %s\n", p.Device.ID, n.Name)
	d, err := o.diskLeaf.AddXFS(ctx, n.ID, *p.Device, p.Force, o.cfg.InstallID)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(r.Log, "disk ready: %s\n", d.MountPath)
	if err := o.pushDisks(ctx, n); err != nil {
		_, _ = fmt.Fprintf(r.Log, "the worker was not told yet (%v); it is retried\n", err)
	}
	return nil
}

func (o *Orchestrator) runDiskGrow(ctx context.Context, r *jobs.Run, p diskJob) error {
	d, has, err := o.nodeDisk(ctx, p.NodeID)
	if err != nil {
		return err
	}
	if !has {
		return errs.ErrNotFound
	}
	d, err = o.diskLeaf.Grow(ctx, d)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(r.Log, "disk grown: %s, device %d bytes\n", d.MountPath, d.DeviceBytes)
	return nil
}

func (o *Orchestrator) runDiskAdd(ctx context.Context, r *jobs.Run, p diskJob) error {
	n, err := o.nodeLeaf().Get(ctx, p.NodeID)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(r.Log, "adding %s on %s\n", p.Path, n.Name)
	d, err := o.diskLeaf.AddPath(ctx, n.ID, p.Path, o.cfg.InstallID)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(r.Log, "disk ready: %s\n", d.MountPath)
	// The worker learns the path now; the facts poll repairs a push that fails.
	if err := o.pushDisks(ctx, n); err != nil {
		_, _ = fmt.Fprintf(r.Log, "the worker was not told yet (%v); it is retried\n", err)
	}
	return nil
}

func (o *Orchestrator) runDiskCheck(ctx context.Context, r *jobs.Run, p diskJob) error {
	d, has, err := o.nodeDisk(ctx, p.NodeID)
	if err != nil {
		return err
	}
	if !has {
		return errs.ErrNotFound
	}
	d, err = o.diskLeaf.Verify(ctx, d)
	if err != nil {
		return err
	}
	if d.State != disk.StateReady {
		return errs.Refusedf("Still missing. Reattach it, then check again.")
	}
	_, _ = fmt.Fprintf(r.Log, "disk ready: %s\n", d.MountPath)
	return o.deployDiskTiles(ctx, r, d)
}

// deployDiskTiles deploys the tiles on the disk's node that have volumes on
// the disk and are not running. A healthy tile is left alone.
func (o *Orchestrator) deployDiskTiles(ctx context.Context, r *jobs.Run, d store.NodeDisk) error {
	fs, err := o.diskLeaf.Folders(ctx, d)
	if err != nil {
		return err
	}
	on := map[string]bool{}
	for _, f := range fs {
		on[f.Name] = true
	}
	es, err := o.store.Environments.ListByNode(ctx, d.NodeID)
	if err != nil {
		return err
	}
	var failed []error
	for _, e := range es {
		ts, err := o.diskTiles(ctx, e, on)
		if err != nil {
			return err
		}
		for _, t := range ts {
			if o.tileRunning(ctx, t) || t.Paused {
				continue
			}
			// stackr keeps no desired state: a stop is the tile's last own job.
			if stopped, err := o.stoppedByUser(ctx, t); err != nil {
				failed = append(failed, err)
				continue
			} else if stopped {
				_, _ = fmt.Fprintf(r.Log, "%s was stopped, left alone\n", t.Slug)
				continue
			}
			if ok, err := o.reachedByRedeploy(ctx, t); err != nil || !ok {
				if err != nil {
					failed = append(failed, err)
				}
				continue
			}
			if _, err := o.Deploy(ctx, t.ID); err != nil {
				failed = append(failed, fmt.Errorf("%s: %w", t.Slug, err))
				continue
			}
			_, _ = fmt.Fprintf(r.Log, "deploying %s\n", t.Slug)
		}
	}
	return errors.Join(failed...)
}

// diskTiles are the env's tiles that mount, or own, a volume with a folder
// on the disk (on holds the folder names).
func (o *Orchestrator) diskTiles(ctx context.Context, e store.Environment, on map[string]bool) ([]Tile, error) {
	vs, err := o.volumes.List(ctx, volume.Scope{Kind: "env", ID: e.ID})
	if err != nil {
		return nil, err
	}
	vs = slices.DeleteFunc(vs, func(v Volume) bool { return v.OrphanedAt != nil || !on[v.Name] })
	if len(vs) == 0 {
		return nil, nil
	}
	ts, err := o.tiles.List(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	// an instance's data volume belongs to its tile, not to a volumes line
	owners := map[string]bool{}
	for _, v := range vs {
		if v.InstanceID != nil {
			it, err := o.instanceTile(ctx, *v.InstanceID)
			if err != nil {
				return nil, err
			}
			owners[it.ID] = true
		}
	}
	return slices.DeleteFunc(ts, func(t Tile) bool {
		return !owners[t.ID] && !slices.ContainsFunc(vs, func(v Volume) bool {
			for _, l := range tile.Lines(t.Volumes) {
				if sl, _, ok := MountedVolume(l); ok && sl == v.Slug {
					return true
				}
			}
			return false
		})
	}), nil
}

// stoppedByUser: of the jobs that start or stop the tile itself (a node-wide
// job such as this check also touches it), the newest is a stop.
func (o *Orchestrator) stoppedByUser(ctx context.Context, t Tile) (bool, error) {
	js, err := o.jobRows.History(ctx, []string{t.ID}, 30)
	if err != nil {
		return false, err
	}
	for _, j := range js {
		switch jobs.Kind(j.Kind) {
		case kindStop:
			return true, nil
		case kindDeploy, kindStart, kindRestart, kindPromote:
			return false, nil
		}
	}
	return false, nil
}
