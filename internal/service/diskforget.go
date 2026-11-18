package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Forget disk, and the two Discard verbs of a ghost volume.

const (
	auditDiskForget        = "disk.forget"
	auditDiskDiscardCopy   = "disk.discard-copy"
	auditDiskDiscardObject = "disk.discard-object"
)

// diskForgetJob is the disk-forget payload.
type diskForgetJob struct {
	DiskID string `json:"disk_id"`
	NodeID string `json:"node_id,omitempty"` // the disk's node: the node drawer finds the job by it
	By     string `json:"by,omitempty"`
	KeyID  string `json:"key_id,omitempty"`
}

// forgetSet is what a forget of one disk touches, read from the folder rows
// (which live until the last step, so a resume sees the same set).
type forgetSet struct {
	folders []store.DiskFolder
	envs    []store.Environment       // envs with a volume on the disk
	vols    map[string][]store.Volume // each affected env's live volumes
	tiles   map[string][]store.Tile   // each affected env's tiles that mount a disk volume
	members []store.OperatedMember    // standbys whose data volume is on the disk
	lock    []string                  // the volume keys of the affected envs
}

func (o *Orchestrator) forgetSet(ctx context.Context, d store.NodeDisk) (forgetSet, error) {
	s := forgetSet{vols: map[string][]store.Volume{}, tiles: map[string][]store.Tile{}}
	fs, err := o.diskLeaf.Folders(ctx, d)
	if err != nil {
		return s, err
	}
	s.folders = fs
	on := map[string]bool{}
	for _, f := range fs {
		on[f.Name] = true
	}
	es, err := o.store.Environments.ListByNode(ctx, d.NodeID)
	if err != nil {
		return s, err
	}
	for _, e := range es {
		vs, err := o.pmVolumes(ctx, e.ID)
		if err != nil {
			return s, err
		}
		if !slices.ContainsFunc(vs, func(v store.Volume) bool { return on[v.Name] }) {
			continue
		}
		ts, err := o.diskTiles(ctx, e, on)
		if err != nil {
			return s, err
		}
		s.envs = append(s.envs, e)
		s.vols[e.ID], s.tiles[e.ID] = vs, ts
		for _, v := range vs {
			s.lock = append(s.lock, "volume:"+v.ID)
		}
	}
	ms, err := o.members.All(ctx)
	if err != nil {
		return s, err
	}
	for _, m := range ms {
		if m.NodeID == d.NodeID && on[m.Volume] {
			s.members = append(s.members, m)
		}
	}
	return s, nil
}

// forgetRefusals are the checks that run before any state change or removal.
func (o *Orchestrator) forgetRefusals(ctx context.Context, d store.NodeDisk, s forgetSet) error {
	on := map[string]bool{}
	for _, f := range s.folders {
		on[f.Name] = true
	}
	// a primary that failed over onto this disk's node is no env's volume here:
	// nothing would restore it, and the way back would start Postgres empty
	for _, m := range s.members {
		if m.Role == opmember.Primary && !m.Fenced {
			name := m.InstanceID
			if it, err := o.instanceTile(ctx, m.InstanceID); err == nil {
				name = it.Name
			}
			return errs.Refusedf("%s runs its primary on this node after a failover. Move it back first.", name)
		}
	}
	var all []store.Volume
	for _, e := range s.envs {
		for _, v := range s.vols[e.ID] {
			all = append(all, v)
			if v.InstanceID == nil || !on[v.Name] {
				continue
			}
			r, ok, err := o.members.LiveReplica(ctx, *v.InstanceID)
			if err != nil {
				return err
			}
			if ok && r.NodeID != d.NodeID {
				return errs.Refusedf("Promote or remove the standby first.")
			}
		}
	}
	return o.noLiveMembers(ctx, all)
}

// liveForgetJob refuses a second press while a forget of this disk is queued
// or running: a same-kind enqueue would cancel it.
func (o *Orchestrator) liveForgetJob(ctx context.Context, diskID string) error {
	js, err := o.jobRows.List(ctx, job.Queued, job.Running, job.Waiting)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(js, func(j store.Job) bool {
		return j.Kind == string(kindDiskForget) && slices.Contains(j.LockSet, "disk:"+diskID)
	}) {
		return errs.Conflictf("A forget of this disk is already running.")
	}
	return nil
}

// ForgetNodeDisk (server admin) forgets a disk that is missing or half added,
// or one whose node is gone; confirm is the node's name or the disk's id. A
// disk of a forgotten node is dropped at once (Job is zero). On a live node a
// job removes the disk's containers and Docker objects, restores each env
// from backups and deletes the row last. On a forgetting row it resumes.
func (o *Orchestrator) ForgetNodeDisk(ctx context.Context, by *Principal, diskID, confirm string) (Job, error) {
	if err := nodeAdmin(by); err != nil {
		return Job{}, err
	}
	d, err := o.store.NodeDisks.Get(ctx, diskID)
	if err != nil {
		return Job{}, err
	}
	n, err := o.store.Nodes.Get(ctx, d.NodeID)
	gone := errors.Is(err, errs.ErrNotFound)
	if err != nil && !gone {
		return Job{}, err
	}
	if err := disk.MayForget(d, gone); err != nil {
		return Job{}, err
	}
	name := n.Name
	if gone {
		name = o.forgottenName(ctx, d.NodeID)
	}
	if confirm != d.ID && (name == "" || confirm != name) {
		return Job{}, errs.Invalidf("confirm", "type %s to confirm", confirmWord(name, d.ID))
	}
	if gone {
		return Job{}, o.forgetGoneDisk(ctx, by, d)
	}
	if _, err := o.diskNodeUp(ctx, n.ID); err != nil {
		return Job{}, err
	}
	if err := o.liveForgetJob(ctx, d.ID); err != nil {
		return Job{}, err
	}
	s, err := o.forgetSet(ctx, d)
	if err != nil {
		return Job{}, err
	}
	if d.State != disk.StateForgetting { // a resume's containers are gone already
		if err := o.forgetRefusals(ctx, d, s); err != nil {
			return Job{}, err
		}
	}
	lock, err := o.nodeLock(ctx, n)
	if err != nil {
		return Job{}, err
	}
	lock = append(append(lock, s.lock...), "disk:"+d.ID)
	return o.enqueueExclusive(ctx, kindDiskForget, diskForgetJob{DiskID: d.ID, NodeID: n.ID, By: by.User.ID, KeyID: by.KeyID},
		[]string{"disk:" + d.ID}, errs.Conflictf("A forget of this disk is already running."), lock...)
}

func confirmWord(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

// forgottenName is the name a forgotten node had, "" when it is not known.
func (o *Orchestrator) forgottenName(ctx context.Context, nodeID string) string {
	fs, err := o.store.ForgottenNodes.List(ctx)
	if err != nil {
		return ""
	}
	for _, f := range fs {
		if f.ID == nodeID {
			return f.Name
		}
	}
	return ""
}

// forgetGoneDisk is the metadata-only forget of a disk whose node is gone.
// It runs only once nothing points at the node any more, which means its
// envs were already recovered; then it is one DELETE, the folder rows going
// with it.
func (o *Orchestrator) forgetGoneDisk(ctx context.Context, by *Principal, d store.NodeDisk) error {
	es, err := o.store.Environments.ListByNode(ctx, d.NodeID)
	if err != nil {
		return err
	}
	vs, err := o.store.Volumes.ListByNode(ctx, d.NodeID)
	if err != nil {
		return err
	}
	ms, err := o.members.All(ctx)
	if err != nil {
		return err
	}
	nm := len(slices.DeleteFunc(slices.Clone(ms), func(m store.OperatedMember) bool { return m.NodeID != d.NodeID }))
	var parts []string
	for _, c := range []struct {
		n    int
		what string
	}{{len(es), "envs"}, {len(vs), "volumes"}, {nm, "standbys"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.what))
		}
	}
	if len(parts) > 0 {
		return errs.Refusedf("%s still point at the forgotten node: recover them first", strings.Join(parts, ", "))
	}
	if err := o.diskLeaf.Delete(ctx, d); err != nil {
		return err
	}
	o.auditDiskLog(ctx, by, auditDiskForget, d.NodeID, d.MountPath)
	return nil
}

// runDiskForget is the "disk-forget" job. The row is the retry key: it says
// forgetting from step 1 and is deleted last, so a crash anywhere is finished
// by pressing the verb again.
func (o *Orchestrator) runDiskForget(ctx context.Context, r *jobs.Run, p diskForgetJob) error {
	by := &Principal{User: User{ID: p.By}, KeyID: p.KeyID}
	d, err := o.store.NodeDisks.Get(ctx, p.DiskID)
	if err != nil {
		return err
	}
	n, err := o.store.Nodes.Get(ctx, d.NodeID)
	if errors.Is(err, errs.ErrNotFound) {
		if err := disk.MayForget(d, true); err != nil {
			return err
		}
		return o.forgetGoneDisk(ctx, by, d)
	}
	if err != nil {
		return err
	}
	if err := disk.MayForget(d, false); err != nil {
		return err
	}
	if _, err := o.diskNodeUp(ctx, n.ID); err != nil {
		return err
	}
	s, err := o.forgetSet(ctx, d)
	if err != nil {
		return err
	}
	if d.State != disk.StateForgetting {
		if err := o.forgetRefusals(ctx, d, s); err != nil {
			return err
		}
	}
	// step 1: from here every volume verb refuses and only this job sees through
	if d, err = o.diskLeaf.BeginForget(ctx, d); err != nil {
		return err
	}
	ctx = disk.WithOp(ctx, d.ID, disk.StateForgetting)
	_, _ = fmt.Fprintf(r.Log, "forgetting the disk at %s on %s\n", d.MountPath, n.Name)

	// step 2: the containers that mount a bound volume; Docker keeps a volume
	// that any container still references
	for _, e := range s.envs {
		for _, t := range s.tiles[e.ID] {
			if err := o.tiles.Teardown(ctx, t, e.Network); err != nil {
				return fmt.Errorf("remove %s: %w", t.Slug, err)
			}
			_, _ = fmt.Fprintf(r.Log, "removed the containers of %s\n", t.Slug)
		}
	}
	for _, m := range s.members {
		nctx := docker.WithNode(ctx, m.NodeID)
		cs, err := o.docker.List(nctx, map[string]string{LabelMember: m.ID})
		if err != nil {
			return err
		}
		for _, c := range cs {
			if err := o.docker.StopRemove(nctx, c.ID); err != nil {
				return err
			}
		}
		if m.Fenced && m.Phase == opmember.Rewind {
			_, _ = fmt.Fprintf(r.Log, "member %s is a fenced old primary in rewind: it does not recover, remove it by hand\n", m.ID)
		}
	}

	// step 3: the bound Docker objects, never a folder
	for _, f := range s.folders {
		dev := docker.DeviceOf(disk.BindOpts(d.MountPath, d.ID, f.Name))
		if gone, err := o.volumes.RemoveBoundTo(ctx, d.NodeID, f.Name, dev); err != nil {
			return fmt.Errorf("remove volume %s: %w", f.Name, err)
		} else if gone {
			_, _ = fmt.Fprintf(r.Log, "removed volume %s (its folder stays)\n", f.Name)
		}
	}

	// step 3a: a container of another stack, or a stale one, may bind a folder
	// below the disk directly; its mount lives in its own namespace and would
	// outlive the host unmount. Checked now, at job time, never on a poll.
	if err := o.containerOnDisk(ctx, d); err != nil {
		return err
	}

	// step 3b: nothing uses the disk any more: an xfs disk comes off the host (unmounted, unit
	// and mount point removed; the device and the folders on it stay). Idempotent for a retry.
	if err := o.diskLeaf.UnmountXFS(ctx, d); err != nil {
		return fmt.Errorf("unmount the disk: %w", err)
	}

	// step 4: the volumes are plain and empty now: restore each env from its
	// newest backup and deploy. Any error, a partial enqueue included, keeps
	// the row forgetting.
	var failed []error
	on := map[string]bool{}
	for _, f := range s.folders {
		on[f.Name] = true
	}
	for _, e := range s.envs {
		// only the disk's volumes: the env's other tiles were not stopped or held
		_, notes, err := o.restoreSomeThenDeploy(ctx, e.ID, s.tiles[e.ID], func(v store.Volume) bool { return on[v.Name] })
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

	// step 5: the row goes last; its folder rows go with it in the same statement
	if err := o.diskLeaf.Delete(ctx, d); err != nil {
		return err
	}
	o.auditDiskLog(ctx, by, auditDiskForget, d.NodeID, d.MountPath)
	if err := o.pushDisks(ctx, n); err != nil {
		_, _ = fmt.Fprintf(r.Log, "the worker was not told yet (%v); it is retried\n", err)
	}
	_, _ = fmt.Fprintf(r.Log, "disk forgotten; the restores run next\n")
	return nil
}

// DiscardPlainCopy (the ghost's resolution) removes only the plain Docker
// object of a volume that also has a folder, and keeps the folder. confirm is
// the volume's name.
func (o *Orchestrator) DiscardPlainCopy(ctx context.Context, by *Principal, volumeID, confirm string) error {
	return o.discardGhost(ctx, by, volumeID, confirm, false)
}

// DiscardBoundObject removes only the Docker object of a volume that is bound
// somewhere other than its folder. Neither folder is touched.
func (o *Orchestrator) DiscardBoundObject(ctx context.Context, by *Principal, volumeID, confirm string) error {
	return o.discardGhost(ctx, by, volumeID, confirm, true)
}

func (o *Orchestrator) discardGhost(ctx context.Context, by *Principal, volumeID, confirm string, bound bool) error {
	if by == nil {
		return errs.ErrRefused
	}
	v, err := o.volumes.Get(ctx, volumeID)
	if err != nil {
		return err
	}
	if err := o.volumeIdle(ctx, v); err != nil {
		return errs.Conflictf("%v", err)
	}
	g, err := o.volumes.DiscardGhost(ctx, v, bound, func(g volume.Ghost) error {
		if confirm == v.Slug {
			return nil
		}
		if bound {
			return errs.Invalidf("confirm", "type %s to remove only the object bound to another folder", v.Slug)
		}
		return errs.Invalidf("confirm", "type %s to remove the plain copy and keep the disk copy", v.Slug)
	})
	if err != nil {
		return err
	}
	action := auditDiskDiscardCopy
	if bound {
		action = auditDiskDiscardObject
	}
	o.auditDiskLog(ctx, by, action, g.Node, g.Name)
	return nil
}

// ForgetPlan is what the Forget dialog shows. Read from rows only, so the
// drawer may build it on every poll. Gone: the disk's node is forgotten and
// only the record goes. Refusal is what Forget would refuse with right now.
type ForgetPlan struct {
	DiskID, Node string // Node is the name the disk's node has or had, "" when not known
	Gone         bool
	Volumes      []AdoptVolume // with their newest backup, when there is one
	Refusal      string
}

// PlanDiskForget reads what forgetting the disk would do.
func (o *Orchestrator) PlanDiskForget(ctx context.Context, diskID string) (ForgetPlan, error) {
	d, err := o.store.NodeDisks.Get(ctx, diskID)
	if err != nil {
		return ForgetPlan{}, err
	}
	p := ForgetPlan{DiskID: d.ID}
	n, err := o.store.Nodes.Get(ctx, d.NodeID)
	p.Gone = errors.Is(err, errs.ErrNotFound)
	if err != nil && !p.Gone {
		return p, err
	}
	p.Node = n.Name
	if p.Gone {
		p.Node = o.forgottenName(ctx, d.NodeID)
	}
	if err := disk.MayForget(d, p.Gone); err != nil {
		p.Refusal = err.Error()
		return p, nil
	}
	if p.Gone {
		p.Refusal, err = o.forgetPointing(ctx, d, p.Node)
		return p, err
	}
	s, err := o.forgetSet(ctx, d)
	if err != nil {
		return p, err
	}
	on := map[string]bool{}
	for _, f := range s.folders {
		on[f.Name] = true
	}
	for _, e := range s.envs {
		for _, v := range s.vols[e.ID] {
			if !on[v.Name] || v.OrphanedAt != nil {
				continue
			}
			lbl := o.moveEnvName(ctx, v)
			av := AdoptVolume{ID: v.ID, Slug: v.Slug, Env: lbl, Where: lbl}
			if b, err := o.newestBackup(ctx, v.ID); err == nil && b != nil {
				av.HasBackup, av.Backup = true, b.CreatedAt
			}
			p.Volumes = append(p.Volumes, av)
		}
	}
	if d.State != disk.StateForgetting {
		if err := o.forgetRefusals(ctx, d, s); err != nil {
			if !answer(err) {
				return p, err
			}
			p.Refusal = o.forgetStandbyText(ctx, d, s, err.Error())
		}
	}
	return p, nil
}

// forgetStandbyText words the standby refusal with the instance it is about:
// "Promote or remove the standby of shop / prod postgres first." Any other
// refusal is returned as it is.
func (o *Orchestrator) forgetStandbyText(ctx context.Context, d store.NodeDisk, s forgetSet, msg string) string {
	if msg != "Promote or remove the standby first." {
		return msg
	}
	on := map[string]bool{}
	for _, f := range s.folders {
		on[f.Name] = true
	}
	for _, e := range s.envs {
		for _, v := range s.vols[e.ID] {
			if v.InstanceID == nil || !on[v.Name] {
				continue
			}
			if r, ok, err := o.members.LiveReplica(ctx, *v.InstanceID); err == nil && ok && r.NodeID != d.NodeID {
				if it, err := o.instanceTile(ctx, *v.InstanceID); err == nil {
					return "Promote or remove the standby of " + o.moveEnvName(ctx, v) + " " + it.Name + " first."
				}
			}
		}
	}
	return msg
}

// forgetPointing is why a record of a forgotten node's disk cannot go yet:
// the envs (volumes, standbys) that still point at the node. "" = none do.
func (o *Orchestrator) forgetPointing(ctx context.Context, d store.NodeDisk, node string) (string, error) {
	es, err := o.store.Environments.ListByNode(ctx, d.NodeID)
	if err != nil {
		return "", err
	}
	vs, err := o.store.Volumes.ListByNode(ctx, d.NodeID)
	if err != nil {
		return "", err
	}
	ms, err := o.members.All(ctx)
	if err != nil {
		return "", err
	}
	nm := len(slices.DeleteFunc(slices.Clone(ms), func(m store.OperatedMember) bool { return m.NodeID != d.NodeID }))
	if len(es)+len(vs)+nm == 0 {
		return "", nil
	}
	var names []string
	for _, e := range es {
		n := e.Name
		if st, err := o.stacks.Get(ctx, e.StackID); err == nil {
			n = st.Name + " / " + e.Name
		}
		names = append(names, n)
	}
	what := ""
	switch {
	case len(names) > 2:
		what = fmt.Sprintf("%s and %d more", strings.Join(names[:2], ", "), len(names)-2)
	case len(names) > 0:
		what = strings.Join(names, ", ")
	case len(vs) > 0:
		what = fmt.Sprintf("%d volumes", len(vs))
	default:
		what = fmt.Sprintf("%d standbys", nm)
	}
	if node == "" {
		node = "the forgotten node"
	}
	return what + " still point at " + node + ". Recover them first.", nil
}

// containerOnDisk refuses while any container on the disk's node, running or
// not, has a mount whose source is the disk path or below it.
func (o *Orchestrator) containerOnDisk(ctx context.Context, d store.NodeDisk) error {
	if d.Kind != disk.KindXFS {
		return nil
	}
	nctx := docker.WithNode(ctx, d.NodeID)
	cs, err := o.docker.List(nctx, nil)
	if err != nil {
		return err
	}
	for _, c := range cs {
		det, err := o.docker.Inspect(nctx, c.ID)
		if errors.Is(err, docker.ErrNotFound) {
			continue // removed since the list
		}
		if err != nil {
			return err
		}
		for _, m := range det.Mounts {
			src, _, _ := strings.Cut(m, " -> ")
			if src == d.MountPath || strings.HasPrefix(src, d.MountPath+"/") {
				return errs.Refusedf("A container still uses this disk: %s. Stop it first.", det.Name)
			}
		}
	}
	return nil
}
