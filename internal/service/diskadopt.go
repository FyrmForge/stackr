package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Adopt: a disk whose node is gone is handed to another node. The disk row is
// the retry key: it says adopting from the moment the operation is recorded
// until the last step, so every ordinary caller is refused in between and a
// crash is finished by pressing Adopt again.

const (
	auditDiskAdopt = "disk.adopt"
)

// DiskAdoptPlan is what the adopt dialog shows. PlanDiskAdopt(node, "") fills
// Candidates and Resume only; with a path it also reads the folder's stamp and
// fills the rest. Refusal is the reason Adopt would refuse right now, "" when
// it would run.
type DiskAdoptPlan struct {
	Node string // the adopting node's name
	Path string

	DiskID  string // the disk the folder's stamp names, "" until it is a known one
	OldNode string // the name the disk's node had, "" when not known

	Envs      []AdoptEnv    // envs that move to the node
	OnDisk    []AdoptVolume // their volumes (and orphans) that have a folder on the disk
	Elsewhere []AdoptVolume // their volumes that are not on the disk: restored from backup, or empty
	Unclaimed []string      // folders no volume or env of the old node claims: kept, never deleted
	Copies    []AdoptCopy   // plain copies already on the node that block the adopt
	Hosts     []AdoptHost   // standbys, or a relocated primary, the old node hosts: they block the adopt
	AdoptedBy string        // the node another adopt of this disk is for, when that is why it refuses

	Refusal string
	// CopiesOnly: the copies are the only thing that refuses, so discarding them lets the adopt run.
	CopiesOnly bool

	// Candidates are the disk rows of forgotten nodes that were never adopted.
	Candidates []AdoptCandidate
	// Resume is the disk of an adopt of this node that was interrupted, with
	// the path it was started on; "" when there is none.
	ResumeDisk, ResumePath string
}

// AdoptEnv is an env that moves.
type AdoptEnv struct {
	ID, Slug string
	Label    string // "shop / prod"
	Tiles    int
}

// AdoptVolume is a volume of an adopted env. For a volume that is not on the
// disk, Backup is the date of the newest backup it comes back from; HasBackup
// false means it starts empty.
type AdoptVolume struct {
	ID, Slug, Env string
	Where         string // "shop / prod"; "" for an orphan
	HasBackup     bool
	Backup        time.Time
}

// AdoptCopy is a plain Docker copy of a volume that has a folder on the
// disk; it holds data and blocks the adopt until the person removes it.
type AdoptCopy struct {
	VolumeID, Slug string
	Bound          bool // bound to a folder other than the disk's, not a plain copy
}

// AdoptHost is an operated instance member that lives on the old node: its
// env ("shop / prod") and its instance tile.
type AdoptHost struct {
	Env, Tile string
	Primary   bool
}

// AdoptCandidate is a disk row of a forgotten node.
type AdoptCandidate struct{ DiskID, Node, Path, State string }

// diskAdoptJob is the disk-adopt payload.
type diskAdoptJob struct {
	DiskID string `json:"disk_id"`
	NodeID string `json:"node_id"` // the new node
	Path   string `json:"path"`
	By     string `json:"by,omitempty"`
	KeyID  string `json:"key_id,omitempty"`
}

// adoptCommitted: the transaction of step 4 has run, so the checks that need
// the old node are not asked again.
func adoptCommitted(d store.NodeDisk) bool {
	return d.State == disk.StateAdopting && d.OpNodeID != "" && d.NodeID == d.OpNodeID
}

// adoptView is what an adopt of d works on, from the rows alone.
type adoptView struct {
	envs    []store.Environment
	vols    map[string][]store.Volume // each env's volumes, orphans included
	all     []store.Volume            // every volume the adopt carries: the envs' and the old node's orphans
	tiles   map[string][]store.Tile
	claimed map[string]bool // folder names a volume of the old node owns
	folders []store.DiskFolder
}

// adoptEnvs are the envs an adopt works on: the persisted set once the
// operation is recorded, else every env still on the old node.
func (o *Orchestrator) adoptEnvs(ctx context.Context, d store.NodeDisk) ([]store.Environment, error) {
	if d.State != disk.StateAdopting {
		return o.store.Environments.ListByNode(ctx, d.NodeID)
	}
	ids, err := opEnvIDs(d)
	if err != nil {
		return nil, err
	}
	var es []store.Environment
	for _, id := range ids {
		e, err := o.store.Environments.Get(ctx, id)
		if errors.Is(err, errs.ErrNotFound) {
			continue // deleted since: nothing of it to restore
		}
		if err != nil {
			return nil, err
		}
		es = append(es, e)
	}
	return es, nil
}

func (o *Orchestrator) adoptView(ctx context.Context, d store.NodeDisk) (adoptView, error) {
	v := adoptView{vols: map[string][]store.Volume{}, tiles: map[string][]store.Tile{}, claimed: map[string]bool{}}
	var err error
	if v.envs, err = o.adoptEnvs(ctx, d); err != nil {
		return v, err
	}
	if v.folders, err = o.diskLeaf.Folders(ctx, d); err != nil {
		return v, err
	}
	for _, e := range v.envs {
		vs, err := o.store.Volumes.ListByScope(ctx, "env", e.ID)
		if err != nil {
			return v, err
		}
		ts, err := o.tiles.List(ctx, e.ID)
		if err != nil {
			return v, err
		}
		v.vols[e.ID], v.tiles[e.ID] = vs, ts
		v.all = append(v.all, vs...)
	}
	orph, err := o.store.Volumes.ListByNode(ctx, d.NodeID)
	if err != nil {
		return v, err
	}
	v.all = append(v.all, orph...)
	for _, x := range v.all {
		v.claimed[x.Name] = true
	}
	if adoptCommitted(d) { // every folder row left is claimed
		for _, f := range v.folders {
			v.claimed[f.Name] = true
		}
	}
	return v, nil
}

// lock is the node lock of the new node, every tile of every env in the set,
// and the volume keys of every env (an env with volumes and no tiles has no
// tile to lock), plus the disk's key.
func (o *Orchestrator) adoptLock(ctx context.Context, n store.Node, d store.NodeDisk, v adoptView) ([]string, error) {
	lock, err := o.nodeLock(ctx, n)
	if err != nil {
		return nil, err
	}
	for _, e := range v.envs {
		for _, t := range v.tiles[e.ID] {
			lock = append(lock, t.ID)
		}
		for _, x := range v.vols[e.ID] {
			lock = append(lock, "volume:"+x.ID)
		}
	}
	lock = append(lock, "disk:"+d.ID)
	sort.Strings(lock)
	return slices.Compact(lock), nil
}

// adoptCopies are the plain Docker copies on the new node of a volume that
// has a folder on the disk and a name the adopt claims. An empty copy is not
// listed: Ensure removes it. A copy whose size is not known counts as data.
func (o *Orchestrator) adoptCopies(ctx context.Context, nodeID, mount, diskID string, v adoptView) ([]AdoptCopy, error) {
	on := map[string]bool{}
	for _, f := range v.folders {
		on[f.Name] = true
	}
	nctx := docker.WithNode(ctx, nodeID)
	var out []AdoptCopy
	for _, x := range v.all {
		if !on[x.Name] || !v.claimed[x.Name] {
			continue
		}
		found, dev, err := o.docker.VolumeDevice(nctx, x.Name)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if dev != "" {
			// bound to the disk's folder is what step 6 wants; bound anywhere
			// else would fail it as a ghost after the commit, forever
			if dev != disk.BindOpts(mount, diskID, x.Name)["device"] {
				out = append(out, AdoptCopy{VolumeID: x.ID, Slug: x.Slug, Bound: true})
			}
			continue
		}
		info, err := o.docker.InspectVolume(nctx, x.Name)
		if errors.Is(err, docker.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.SizeBytes != 0 {
			out = append(out, AdoptCopy{VolumeID: x.ID, Slug: x.Slug})
		}
	}
	return out, nil
}

// adoptRefusals are the checks that write nothing. They run on every press
// while the row's node is still the old one.
func (o *Orchestrator) adoptRefusals(ctx context.Context, d store.NodeDisk, n store.Node, path string, v adoptView) error {
	if err := o.adoptBlocks(ctx, d, n, path, v); err != nil {
		return err
	}
	return o.adoptCopyBlock(ctx, d, n, path, v)
}

// adoptBlocks are the refusals that are not about plain copies on the node.
func (o *Orchestrator) adoptBlocks(ctx context.Context, d store.NodeDisk, n store.Node, path string, v adoptView) error {
	if err := o.panelDBDiskRefusal(d); err != nil {
		return err
	}
	if err := o.observeDiskRefusal(ctx, d.NodeID); err != nil {
		return err
	}
	switch d.State {
	case disk.StateReady, disk.StateMissing:
	case disk.StateAdopting:
		if d.OpNodeID != n.ID || d.OpMountPath != path {
			// an adopt whose target is gone can never commit: another node takes it over
			if gone, err := o.adoptTargetGone(ctx, d); err != nil {
				return err
			} else if !gone {
				return errs.Refusedf("This disk is already being adopted for another node or path. Finish that adopt first.")
			}
		}
	default:
		return errs.Refusedf("This disk cannot be adopted while it is %s.", d.State)
	}
	if d.NodeID == n.ID {
		return errs.Refusedf("This disk is already on this node.")
	}
	old, err := o.store.Nodes.Get(ctx, d.NodeID)
	switch {
	case err == nil && old.Status == node.StatusOnline:
		return errs.Refusedf("The disk's node, %s, is still online. A disk can only be adopted once its node is gone.", old.Name)
	case err == nil:
		// a folder (NFS, a bind mount) can be mounted by two hosts at once, and
		// nothing fences an offline node's device either: only a forgotten node
		// (no row) releases its disk
		return errs.Refusedf("The disk's node, %s, is offline but not forgotten. Forget the node first: it may still be writing to the disk.", old.Name)
	case !errors.Is(err, errs.ErrNotFound):
		return err
	}
	if cur, has, err := o.nodeDisk(ctx, n.ID); err != nil {
		return err
	} else if has && cur.ID != d.ID {
		return errs.Conflictf("This node already has a disk.")
	}
	ms, err := o.members.All(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(ms, func(m store.OperatedMember) bool { return m.NodeID == d.NodeID }) {
		return errs.Refusedf("Remove the standbys first.")
	}
	var live []store.Volume
	for _, e := range v.envs {
		live = append(live, slices.DeleteFunc(slices.Clone(v.vols[e.ID]), func(x store.Volume) bool { return x.OrphanedAt != nil })...)
	}
	return o.noLiveMembers(ctx, live)
}

// adoptCopyBlock refuses while a volume has a copy on the node in Docker storage.
func (o *Orchestrator) adoptCopyBlock(ctx context.Context, d store.NodeDisk, n store.Node, path string, v adoptView) error {
	cs, err := o.adoptCopies(ctx, n.ID, path, d.ID, v)
	if err != nil {
		return err
	}
	if len(cs) > 0 && cs[0].Bound {
		return errs.Refusedf("%s has a Docker volume on this node bound to another folder. Remove that volume first.", cs[0].Slug)
	}
	if len(cs) > 0 {
		return errs.Refusedf("%s has a plain copy on this node. Remove or keep the plain copy first.", cs[0].Slug)
	}
	return nil
}

// adoptTargetGone: the adopt has not committed and the node it was queued for
// no longer has a row, so only another node can finish it.
func (o *Orchestrator) adoptTargetGone(ctx context.Context, d store.NodeDisk) (bool, error) {
	if d.State != disk.StateAdopting || adoptCommitted(d) {
		return false, nil
	}
	_, err := o.store.Nodes.Get(ctx, d.OpNodeID)
	if errors.Is(err, errs.ErrNotFound) {
		return true, nil
	}
	return false, err
}

// liveAdoptJob refuses a second press while an adopt of this disk is queued
// or running: a same-kind enqueue would cancel it.
func (o *Orchestrator) liveAdoptJob(ctx context.Context, diskID string) error {
	js, err := o.jobRows.List(ctx, job.Queued, job.Running, job.Waiting)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(js, func(j store.Job) bool {
		return j.Kind == string(kindDiskAdopt) && slices.Contains(j.LockSet, "disk:"+diskID)
	}) {
		return errs.Conflictf("An adopt of this disk is already running.")
	}
	return nil
}

// adoptStamp reads the folder's stamp on the new node: it must name this disk.
func (o *Orchestrator) adoptStamp(ctx context.Context, nodeID, path string) (hostdisk.Fact, error) {
	f, err := o.diskLeaf.Probe(ctx, nodeID, path)
	var he *hostdisk.Error
	if errors.As(err, &he) {
		// An xfs disk is not mounted on the new node until the adopt runs: the
		// stamp cannot be read yet, so the device with the disk's filesystem
		// stands for it.
		if id, ok := xfsMountID(path); ok && slices.Contains([]string{hostdisk.CodeNotMountPoint, hostdisk.CodeDiskMissing, hostdisk.CodeRootFS}, he.Code) {
			return o.xfsStamp(ctx, nodeID, id, path)
		}
		if slices.Contains(probeRefusals, he.Code) {
			return f, errs.Refusedf("%s", he.Message)
		}
	}
	return f, err
}

// xfsMountID is the disk id of a path that is an xfs mount point.
func xfsMountID(path string) (string, bool) {
	id, ok := strings.CutPrefix(path, hostdisk.XFSRoot+"/")
	return id, ok && id != "" && !strings.Contains(id, "/")
}

// xfsStamp is the stamp of an xfs disk that is not mounted on the node yet:
// the disk's id when the node lists a device carrying its filesystem, and
// matches it by the filesystem uuid. A disk this panel does not know, or a
// path disk, gives an empty stamp.
func (o *Orchestrator) xfsStamp(ctx context.Context, nodeID, diskID, path string) (hostdisk.Fact, error) {
	d, err := o.store.NodeDisks.Get(ctx, diskID)
	if errors.Is(err, errs.ErrNotFound) || err == nil && d.Kind != disk.KindXFS {
		return hostdisk.Fact{}, nil
	}
	if err != nil {
		return hostdisk.Fact{}, err
	}
	has, err := o.diskLeaf.HasDevice(ctx, nodeID, d)
	if err != nil {
		return hostdisk.Fact{}, err
	}
	if !has {
		return hostdisk.Fact{}, errs.Refusedf("The disk's device is not on this node. Attach it, then try again.")
	}
	return hostdisk.Fact{Path: path, StampID: d.ID, FSType: "xfs"}, nil
}

// oldNodeName is the name of the node a disk was on: its row's, or the one a
// forgotten node had.
func (o *Orchestrator) oldNodeName(ctx context.Context, nodeID string) string {
	if n, err := o.store.Nodes.Get(ctx, nodeID); err == nil {
		return n.Name
	}
	return o.forgottenName(ctx, nodeID)
}

// adoptCandidates are the disk rows of forgotten nodes (no node row) that
// were never adopted. An offline node is not enough, for any kind: nothing
// fences it, so it may still be writing to the disk.
func (o *Orchestrator) adoptCandidates(ctx context.Context) ([]AdoptCandidate, error) {
	rows, err := o.store.NodeDisks.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []AdoptCandidate
	for _, r := range rows {
		if r.State == disk.StateForgetting || r.State == disk.StateAdding || adoptCommitted(r) {
			continue
		}
		if _, err := o.store.Nodes.Get(ctx, r.NodeID); err == nil {
			continue
		} else if !errors.Is(err, errs.ErrNotFound) {
			return nil, err
		}
		out = append(out, AdoptCandidate{DiskID: r.ID, Node: o.oldNodeName(ctx, r.NodeID), Path: r.MountPath, State: r.State})
	}
	return out, nil
}

// PlanDiskAdopt reads what adopting the disk in the folder at path on the
// node would do, and changes nothing. With no path it lists the disks that
// could be adopted. The folder's stamp says which disk it is.
func (o *Orchestrator) PlanDiskAdopt(ctx context.Context, nodeID, path string) (DiskAdoptPlan, error) {
	n, err := o.diskNodeUp(ctx, nodeID)
	if err != nil {
		return DiskAdoptPlan{}, err
	}
	p := DiskAdoptPlan{Node: n.Name, Path: path}
	if p.Candidates, err = o.adoptCandidates(ctx); err != nil {
		return p, err
	}
	cur, has, err := o.nodeDisk(ctx, n.ID)
	if err != nil {
		return p, err
	}
	if has && cur.State == disk.StateAdopting && cur.OpNodeID == n.ID {
		p.ResumeDisk, p.ResumePath = cur.ID, cur.OpMountPath
	}
	if path == "" {
		return p, nil
	}
	if err := diskPath(path); err != nil {
		return p, err
	}
	if _, xfs := xfsMountID(path); !xfs { // an xfs disk is mounted by the helper, the panel sees it through its bind
		if err := o.folderDiskOK(ctx, n.ID); err != nil {
			return p, err
		}
	} else if p.ResumeDisk == "" { // an adopt already under way resumes
		if err := o.deviceDiskOK(ctx, n); err != nil {
			return p, err
		}
	}
	f, err := o.adoptStamp(ctx, n.ID, path)
	if err != nil {
		if answer(err) {
			p.Refusal = err.Error()
			return p, nil
		}
		return p, err
	}
	d, err := o.store.NodeDisks.Get(ctx, f.StampID)
	if errors.Is(err, errs.ErrNotFound) {
		p.Refusal = "This folder carries no disk this panel knows."
		return p, nil
	}
	if err != nil {
		return p, err
	}
	p.DiskID, p.OldNode = d.ID, o.oldNodeName(ctx, d.NodeID)
	v, err := o.adoptView(ctx, d)
	if err != nil {
		return p, err
	}
	o.adoptFill(ctx, &p, v)
	if p.Copies, err = o.adoptCopies(ctx, n.ID, path, d.ID, v); err != nil {
		return p, err
	}
	if !adoptCommitted(d) {
		err := o.adoptBlocks(ctx, d, n, path, v)
		if err == nil {
			err = o.adoptCopyBlock(ctx, d, n, path, v)
			p.CopiesOnly = err != nil && len(p.Copies) > 0
		}
		if err != nil {
			if !answer(err) {
				return p, err
			}
			p.Refusal = err.Error()
			if p.Refusal == "Remove the standbys first." {
				p.Hosts = o.adoptHosts(ctx, d)
			}
			if d.State == disk.StateAdopting && d.OpNodeID != n.ID {
				if on, err := o.store.Nodes.Get(ctx, d.OpNodeID); err == nil {
					p.AdoptedBy = on.Name
				}
			}
		}
	}
	return p, nil
}

// adoptHosts are the members on the disk's old node, each named by its instance.
func (o *Orchestrator) adoptHosts(ctx context.Context, d store.NodeDisk) []AdoptHost {
	ms, err := o.members.All(ctx)
	if err != nil {
		return nil
	}
	var out []AdoptHost
	for _, m := range ms {
		if m.NodeID != d.NodeID {
			continue
		}
		it, err := o.instanceTile(ctx, m.InstanceID)
		if err != nil {
			continue
		}
		out = append(out, AdoptHost{Env: o.envLabel(ctx, it.EnvironmentID), Tile: it.Name, Primary: m.Role == opmember.Primary})
	}
	return out
}

// answer: a refusal the plan shows as its Refusal rather than fails with.
func answer(err error) bool {
	_, conflict := errs.IsConflict(err)
	return conflict || errors.Is(err, errs.ErrRefused)
}

// adoptFill sets the lists of the plan from the view.
func (o *Orchestrator) adoptFill(ctx context.Context, p *DiskAdoptPlan, v adoptView) {
	on := map[string]bool{}
	for _, f := range v.folders {
		on[f.Name] = true
	}
	for _, e := range v.envs {
		label := o.envLabel(ctx, e.ID)
		p.Envs = append(p.Envs, AdoptEnv{ID: e.ID, Slug: e.Slug, Label: label, Tiles: len(v.tiles[e.ID])})
		for _, x := range v.vols[e.ID] {
			av := AdoptVolume{ID: x.ID, Slug: x.Slug, Env: e.Slug, Where: label}
			switch {
			case on[x.Name]:
				p.OnDisk = append(p.OnDisk, av)
			case x.OrphanedAt == nil:
				if b, err := o.newestBackup(ctx, x.ID); err == nil && b != nil {
					av.HasBackup, av.Backup = true, b.CreatedAt
				}
				p.Elsewhere = append(p.Elsewhere, av)
			}
		}
	}
	for _, x := range v.all {
		if on[x.Name] && !slices.ContainsFunc(p.OnDisk, func(a AdoptVolume) bool { return a.ID == x.ID }) {
			p.OnDisk = append(p.OnDisk, AdoptVolume{ID: x.ID, Slug: x.Slug})
		}
	}
	for _, f := range v.folders {
		if !v.claimed[f.Name] && f.Name != hostdisk.PanelDBFolder { // the panel database folder is never offered back
			p.Unclaimed = append(p.Unclaimed, f.Name)
		}
	}
}

// AdoptDisk (server admin) makes the disk diskID, mounted at path on the node,
// that node's disk, carrying the envs of its old node over. confirm is the
// node's name or the disk's id; a disk already adopting resumes without it.
func (o *Orchestrator) AdoptDisk(ctx context.Context, by *Principal, nodeID, diskID, path, confirm string) (Job, error) {
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
	d, err := o.store.NodeDisks.Get(ctx, diskID)
	if err != nil {
		return Job{}, err
	}
	if d.Kind == disk.KindXFS {
		if path != hostdisk.MountPoint(d.ID) {
			return Job{}, errs.Refusedf("An xfs disk is mounted at %s.", hostdisk.MountPoint(d.ID))
		}
		if d.State != disk.StateAdopting { // an adopt already under way resumes
			if err := o.deviceDiskOK(ctx, n); err != nil {
				return Job{}, err
			}
		}
	} else if err := o.folderDiskOK(ctx, n.ID); err != nil {
		return Job{}, err
	}
	if d.State != disk.StateAdopting && confirm != d.ID && confirm != n.Name {
		return Job{}, errs.Invalidf("confirm", "type %s to confirm", n.Name)
	}
	if err := o.liveAdoptJob(ctx, d.ID); err != nil {
		return Job{}, err
	}
	v, err := o.adoptView(ctx, d)
	if err != nil {
		return Job{}, err
	}
	if !adoptCommitted(d) {
		if err := o.adoptRefusals(ctx, d, n, path, v); err != nil {
			return Job{}, err
		}
		f, err := o.adoptStamp(ctx, n.ID, path)
		if err != nil {
			return Job{}, err
		}
		if f.StampID != d.ID {
			return Job{}, errs.Refusedf("This folder carries another disk, not the one to adopt.")
		}
	} else if path != d.MountPath {
		return Job{}, errs.Refusedf("This disk is being adopted at %s.", d.MountPath)
	}
	if o.diskWorker(n) {
		if f := o.diskFacts(ctx, n); f == nil || !f.DisksRead {
			return Job{}, errs.Refusedf("This node's worker is too old to manage a disk. Update it first.")
		}
	}
	lock, err := o.adoptLock(ctx, n, d, v)
	if err != nil {
		return Job{}, err
	}
	return o.enqueueExclusive(ctx, kindDiskAdopt, diskAdoptJob{DiskID: d.ID, NodeID: n.ID, Path: path, By: by.User.ID, KeyID: by.KeyID},
		[]string{"disk:" + d.ID}, errs.Conflictf("An adopt of this disk is already running."), lock...)
}

// DiscardAdoptCopy removes a plain copy that blocks an adopt, before the
// adopt has started. The volume's env is still on the old node then, so the
// Discard verb of a ghost cannot reach it. Only the Docker object on the new
// node goes: the folder stays. path is the folder the adopt was asked for.
// confirm is the volume's slug.
func (o *Orchestrator) DiscardAdoptCopy(ctx context.Context, by *Principal, nodeID, path, diskID, volumeID, confirm string) error {
	if err := nodeAdmin(by); err != nil {
		return err
	}
	n, err := o.diskNodeUp(ctx, nodeID)
	if err != nil {
		return err
	}
	d, err := o.store.NodeDisks.Get(ctx, diskID)
	if err != nil {
		return err
	}
	if adoptCommitted(d) {
		return errs.Refusedf("The adopt has started. Resume it, then remove the copy from the volume.")
	}
	v, err := o.adoptView(ctx, d)
	if err != nil {
		return err
	}
	if err := o.adoptBlocks(ctx, d, n, path, v); err != nil {
		return err
	}
	cs, err := o.adoptCopies(ctx, n.ID, path, d.ID, v)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(cs, func(c AdoptCopy) bool { return c.VolumeID == volumeID })
	if i < 0 {
		return errs.Refusedf("This volume has no plain copy on this node.")
	}
	if confirm != cs[i].Slug {
		return errs.Invalidf("confirm", "type %s to remove the plain copy and keep the disk copy", cs[i].Slug)
	}
	x, err := o.store.Volumes.Get(ctx, volumeID)
	if err != nil {
		return err
	}
	if err := o.docker.RemoveVolume(docker.WithNode(ctx, n.ID), x.Name); err != nil {
		return err
	}
	o.volumes.ForgetGhost(n.ID, x.Name)
	o.auditDiskLog(ctx, by, auditDiskDiscardCopy, n.ID, x.Name)
	return nil
}

// runDiskAdopt is the "disk-adopt" job; see the plan's Adopt job steps. The
// row says adopting from step 2 and goes ready last, so a crash anywhere is
// finished by pressing the verb again.
func (o *Orchestrator) runDiskAdopt(ctx context.Context, r *jobs.Run, p diskAdoptJob) error {
	by := &Principal{User: User{ID: p.By}, KeyID: p.KeyID}
	d, err := o.store.NodeDisks.Get(ctx, p.DiskID)
	if err != nil {
		return err
	}
	n, err := o.diskNodeUp(ctx, p.NodeID)
	if err != nil {
		return err
	}
	// The lock set was fixed when the press queued the job; what the job reads
	// now must lie inside it, or an env or volume it would touch was never held.
	if lv, err := o.adoptView(ctx, d); err != nil {
		return err
	} else if lock, err := o.adoptLock(ctx, n, d, lv); err != nil {
		return err
	} else if have := r.Job.LockSet; len(have) > 0 && slices.ContainsFunc(lock, func(k string) bool { return !slices.Contains(have, k) }) {
		return errs.Conflictf("The disk's envs or volumes changed since the adopt was queued. Press Adopt again.")
	}
	if !adoptCommitted(d) {
		v, err := o.adoptView(ctx, d)
		if err != nil {
			return err
		}
		if err := o.adoptRefusals(ctx, d, n, p.Path, v); err != nil {
			return err
		}
		// an xfs disk is mounted on the new node first (idempotent, a resume does it again)
		if d.Kind == disk.KindXFS {
			if p.Path != hostdisk.MountPoint(d.ID) {
				return errs.Refusedf("An xfs disk is mounted at %s.", hostdisk.MountPoint(d.ID))
			}
			if err := o.diskLeaf.MountXFS(ctx, n.ID, d, p.Path); err != nil {
				return err
			}
		}
		// step 2: the operation is recorded; from here the new node sees its disk as not ready
		ids := make([]string, 0, len(v.envs))
		for _, e := range v.envs {
			ids = append(ids, e.ID)
		}
		if gone, err := o.adoptTargetGone(ctx, d); err != nil {
			return err
		} else if gone && (d.OpNodeID != n.ID || d.OpMountPath != p.Path) {
			d, err = o.diskLeaf.Retarget(ctx, d, n.ID, p.Path)
			if err != nil {
				return err
			}
		} else if d, err = o.diskLeaf.BeginAdopt(ctx, d.ID, n.ID, p.Path, ids); err != nil {
			return err
		}
	}
	ctx = disk.WithOp(ctx, d.ID, disk.StateAdopting)
	_, _ = fmt.Fprintf(r.Log, "adopting the disk at %s on %s\n", p.Path, n.Name)

	if !adoptCommitted(d) {
		// step 3: the stamp's owner becomes the new node. A refusal wrote nothing, so the
		// row goes back to not ready; any other failure keeps it adopting for a retry.
		if err := o.claimDisk(ctx, d); err != nil {
			var he *hostdisk.Error
			if errors.As(err, &he) && he.Code != hostdisk.CodeIO {
				_ = o.diskLeaf.AbortAdopt(context.WithoutCancel(ctx), d)
			}
			return err
		}
		// step 4: one transaction
		if d, err = o.adoptCommit(ctx, d, n, r); err != nil {
			return err
		}
	}

	// step 5: the mesh and the worker's registry
	o.kickMesh()
	if err := o.pushDisks(ctx, n); err != nil {
		_, _ = fmt.Fprintf(r.Log, "the worker was not told yet (%v); it is retried\n", err)
	}

	v, err := o.adoptView(ctx, d)
	if err != nil {
		return err
	}
	// step 6: the Docker objects of every folder row left
	on := map[string]bool{}
	for _, f := range v.folders {
		on[f.Name] = true
	}
	for _, x := range v.all {
		if !on[x.Name] {
			continue
		}
		if _, err := o.volumes.Ensure(ctx, x); err != nil {
			return fmt.Errorf("make volume %s: %w", x.Slug, err)
		}
	}

	// step 7: restore and deploy each env of the persisted set. Any error, a
	// partial enqueue failure included, keeps the row adopting.
	var failed []error
	for _, e := range v.envs {
		if e.NodeID != n.ID { // not carried over by the commit: nothing to restore here
			continue
		}
		_, notes, err := o.restoreThenDeploy(ctx, e.ID, v.tiles[e.ID])
		for _, note := range notes {
			_, _ = fmt.Fprintf(r.Log, "%s: %s\n", e.Slug, note)
		}
		if err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", e.Slug, err))
		}
	}
	if err := errors.Join(failed...); err != nil {
		return err
	}

	// step 8: only now does the row go ready
	if err := o.diskLeaf.FinishAdopt(ctx, d); err != nil {
		return err
	}
	o.auditDiskLog(ctx, by, auditDiskAdopt, n.ID, p.Path)
	_, _ = fmt.Fprintf(r.Log, "disk adopted; the restores run next\n")
	return nil
}

// claimDisk is step 3. A stamp owned by an earlier target of this adopt that
// is gone now (it died after its claim) is taken over from that node.
func (o *Orchestrator) claimDisk(ctx context.Context, d store.NodeDisk) error {
	if f, err := o.adoptStamp(ctx, d.OpNodeID, d.OpMountPath); err == nil && f.StampID == d.ID &&
		f.StampOwner != "" && f.StampOwner != d.OpFromNodeID && f.StampOwner != d.OpNodeID {
		if _, err := o.store.Nodes.Get(ctx, f.StampOwner); errors.Is(err, errs.ErrNotFound) {
			return o.diskLeaf.ClaimFrom(ctx, d, f.StampOwner)
		}
	}
	return o.diskLeaf.Claim(ctx, d)
}

// adoptCommit is step 4: the disk, its folder rows, the envs, the placements
// and the orphans move to the new node in one transaction. The claim set is
// read inside it, before the repoint.
func (o *Orchestrator) adoptCommit(ctx context.Context, d store.NodeDisk, n store.Node, r *jobs.Run) (store.NodeDisk, error) {
	from := d.OpFromNodeID
	ids, err := opEnvIDs(d)
	if err != nil {
		return d, err
	}
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		// exactly the envs the operation recorded, each still on the old node:
		// an env made or moved since was never locked and never restored
		var es []store.Environment
		for _, id := range ids {
			e, err := tx.Environments.Get(ctx, id)
			if errors.Is(err, errs.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if e.NodeID == from {
				es = append(es, e)
			}
		}
		claimed := map[string]bool{}
		for _, e := range es {
			vs, err := tx.Volumes.ListByScope(ctx, "env", e.ID)
			if err != nil {
				return err
			}
			for _, x := range vs {
				claimed[x.Name] = true
			}
		}
		orph, err := tx.Volumes.ListByNode(ctx, from)
		if err != nil {
			return err
		}
		for _, x := range orph {
			claimed[x.Name] = true
		}
		var dropped []string
		if d, dropped, err = disk.Repoint(ctx, tx, d, claimed); err != nil {
			return err
		}
		if len(dropped) > 0 {
			_, _ = fmt.Fprintf(r.Log, "folders nothing claims stay on the disk, unused: %s\n", strings.Join(dropped, ", "))
		}
		for _, e := range es {
			if err := tx.Environments.SetNode(ctx, e.ID, n.ID); err != nil {
				return err
			}
		}
		ps, err := tx.Placements.ListByNode(ctx, from)
		if err != nil {
			return err
		}
		for _, p := range ps {
			if err := tx.Placements.SetNode(ctx, p.ID, n.ID); err != nil {
				return err
			}
		}
		return tx.Volumes.RepointNode(ctx, from, n.ID)
	})
	return d, err
}
