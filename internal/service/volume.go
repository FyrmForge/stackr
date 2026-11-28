package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	Volume      = store.Volume
	VolumeScope = volume.Scope
)

func (o *Orchestrator) Volumes(ctx context.Context, s VolumeScope) ([]Volume, error) {
	return o.volumes.List(ctx, s)
}

// DeclareVolume makes (or adopts an orphaned) volume in a scope.
func (o *Orchestrator) DeclareVolume(ctx context.Context, s VolumeScope, slug string) (Volume, error) {
	v, _, err := o.volumes.Declare(ctx, s, slug, nil)
	return v, err
}

// DeleteVolume removes a volume at once: its data, its row and its schedules.
// Its backup runs and the archives stay in their destinations (the run rows
// lose their volume). Refused while a tile mounts it, while an operated
// instance owns it and while a job holds it. backupFirst backs it up first as
// a job (the returned Job); a failed backup deletes nothing. Without it the
// delete is done when the call returns (zero Job). On a forgotten node only
// the row goes: there is no Docker volume left to reach.
func (o *Orchestrator) DeleteVolume(ctx context.Context, id string, backupFirst bool) (Job, error) {
	v, err := o.volumes.Get(ctx, id)
	if err != nil {
		return Job{}, err
	}
	return o.deleteVolume(ctx, v, backupFirst)
}

func (o *Orchestrator) deleteVolume(ctx context.Context, v Volume, backupFirst bool) (Job, error) {
	by, gone, err := o.deleteChecks(ctx, v)
	if err != nil {
		return Job{}, err
	}
	if backupFirst && gone {
		return Job{}, errs.Refusedf("The node of %s is forgotten, so there is nothing to back up. Delete it without Back up first.", v.Slug)
	}
	if err := o.volumeBusy(ctx, v); err != nil {
		return Job{}, err
	}
	if !backupFirst {
		return Job{}, o.deleteVolumeNow(ctx, v, by, gone)
	}
	if err := o.volumeNodeUp(ctx, v.ID); err != nil {
		return Job{}, err
	}
	d, err := o.BackupFirstDest(ctx, v.ID)
	if err != nil {
		return Job{}, err
	}
	return o.enqueueExclusive(ctx, kindVolumeDelete,
		volumeDataJob{VolumeID: v.ID, BackupFirst: true, DestID: destArg(d)},
		[]string{"volume:" + v.ID}, errs.Conflictf("%s is already being deleted.", v.Slug), "volume:"+v.ID)
}

// deleteChecks are the refusals of an immediate delete, all before anything
// changes: the slugs of the tiles that still mount it, and whether its node
// is forgotten (then only the row goes).
func (o *Orchestrator) deleteChecks(ctx context.Context, v Volume) (by []string, gone bool, err error) {
	if v.InstanceID != nil {
		return nil, false, errs.Conflictf("%s belongs to an operated instance; it goes when the instance does", v.Slug)
	}
	if v.OrphanedAt != nil && OldCopyHeld(*v.OrphanedAt) {
		return nil, false, errs.Conflictf("%s is the old copy of a move to a disk; it goes when the move is done", v.Slug)
	}
	ts, err := o.mounters(ctx, v)
	if err != nil {
		return nil, false, err
	}
	// a tile whose delete is queued or running lets go: right after a
	// tile delete, its row is still there for a few seconds
	for _, t := range ts {
		j, ok, err := o.jobRows.Last(ctx, t.ID)
		if err != nil {
			return nil, false, err
		}
		if !ok || j.Kind != string(kindDelete) || !job.Live(j.State) {
			by = append(by, t.Slug)
		}
	}
	if err := volume.CheckDetached(by); err != nil {
		return by, false, err
	}
	gone, err = o.pmForgotten(ctx, o.volumeNode(ctx, v.ID))
	return by, gone, err
}

// volumeBusy refuses while a queued or running job holds the volume (a
// backup, restore, wipe, delete or move).
func (o *Orchestrator) volumeBusy(ctx context.Context, v Volume) error {
	if err := o.moveLive(ctx, []Volume{v}); err != nil {
		return err
	}
	live, err := o.jobRows.List(ctx, job.Queued, job.Waiting, job.Running)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(live, func(j store.Job) bool { return slices.Contains(j.LockSet, "volume:"+v.ID) }) {
		return errs.Conflictf("%s is busy with a job. Wait for it to finish.", v.Slug)
	}
	return nil
}

// deleteVolumeNow removes the Docker volume (or disk folder) and the row,
// then the schedules' cron entries and alerts: the foreign key drops the
// schedule rows without telling the scheduler. rowOnly is the forgotten node.
func (o *Orchestrator) deleteVolumeNow(ctx context.Context, v Volume, by []string, rowOnly bool) error {
	ss, err := o.backups.Schedules(ctx, v.ID)
	if err != nil {
		return err
	}
	del := o.volumes.Delete
	if rowOnly {
		del = o.volumes.DeleteRow
	}
	if err := del(ctx, v, by); err != nil {
		return err
	}
	if o.sched != nil {
		o.sched.Reload(ctx)
	}
	keys := []string{staleKey(v.ID)}
	for _, s := range ss {
		keys = append(keys, "alert.backup.schedule."+s.ID)
	}
	for _, k := range keys {
		_ = o.store.Settings.Delete(ctx, k)
		o.clearProblem(ctx, k)
	}
	return nil
}

// RestoreVolume takes a removed volume back: the one a stack file dropped
// (its data is kept for the retention window). Declaring the name again does
// the same.
func (o *Orchestrator) RestoreVolume(ctx context.Context, id string) error {
	v, err := o.volumes.Get(ctx, id)
	if err != nil {
		return err
	}
	switch {
	case v.OrphanedAt == nil:
		return errs.Conflictf("%s is not removed.", v.Slug)
	case OldCopyHeld(*v.OrphanedAt):
		return errs.Conflictf("%s is the old copy of a move to a disk and cannot be restored", v.Slug)
	case v.InstanceID != nil:
		return errs.Conflictf("%s belonged to an operated instance and cannot be restored", v.Slug)
	}
	if _, err := o.scopeOrg(ctx, v.ScopeKind, v.ScopeID); err != nil {
		return err // its environment or stack is gone too
	}
	_, err = o.volumes.Restore(ctx, v)
	return err
}

// MountedVolume reads a volumes line that names an env volume: its slug and
// the container path. host: and share: lines (and lines that do not parse)
// name none.
func MountedVolume(line string) (slug, path string, ok bool) {
	m, err := tile.ParseMount(strings.TrimSpace(line))
	if err != nil || m.Kind != tile.MountVolume {
		return "", "", false
	}
	return m.Volume, m.Path, true
}

// mounters are the tiles whose volumes lines name an env volume.
func (o *Orchestrator) mounters(ctx context.Context, v Volume) ([]Tile, error) {
	if v.ScopeKind != "env" {
		return nil, nil
	}
	ts, err := o.tiles.List(ctx, v.ScopeID)
	var out []Tile
	for _, t := range ts {
		for _, l := range tile.Lines(t.Volumes) {
			if sl, _, ok := MountedVolume(l); ok && sl == v.Slug {
				out = append(out, t)
				break
			}
		}
	}
	return out, err
}

// maxCapMB is 1 PB in MB, far inside int64 bytes.
const maxCapMB = 1 << 30

// capBytes is a volume's cap in bytes; 0 means none.
func capBytes(mb *int64) int64 {
	if mb == nil || *mb <= 0 {
		return 0
	}
	return *mb << 20
}

// folderCap is the cap in bytes of the volume with this Docker name; the disk
// leaf asks it whenever it puts a folder on the host, so a cap survives a
// re-place. A volume this panel has no row for has none.
// ponytail: a scan of the volume rows per call; a by-name lookup if volumes ever number in the thousands.
func (o *Orchestrator) folderCap(ctx context.Context, name string) (int64, error) {
	for _, list := range []func(context.Context) ([]store.Volume, error){o.store.Volumes.ListLive, o.store.Volumes.ListOrphaned} {
		vs, err := list(ctx)
		if err != nil {
			return 0, err
		}
		for _, v := range vs {
			if v.Name == name {
				return capBytes(v.MaxSizeMB), nil
			}
		}
	}
	return 0, nil
}

// xfsDiskOf is the xfs disk a volume's folder is on, when it has one.
func (o *Orchestrator) xfsDiskOf(ctx context.Context, v Volume) (store.NodeDisk, bool, error) {
	rt, node, err := o.volumes.Where(ctx, v)
	if err != nil || rt != volume.RouteDisk {
		return store.NodeDisk{}, false, err
	}
	d, ok, err := o.nodeDisk(ctx, node)
	return d, ok && d.Kind == disk.KindXFS, err
}

// CapEnforced says whether a volume's cap is a limit: only a volume with a
// folder on an xfs disk has one. On any other volume the cap is saved and
// does nothing (rows only, no host call).
func (o *Orchestrator) CapEnforced(ctx context.Context, v Volume) (bool, error) {
	_, ok, err := o.xfsDiskOf(ctx, v)
	return ok, err
}

// CapPending says whether an xfs volume's saved cap may not be on its folder
// yet (the host did not confirm it). A memory read.
func (o *Orchestrator) CapPending(v Volume) bool {
	return o.diskLeaf.CapPending(v.Name)
}

// CheckVolumeCap is the refusal a size cap gets before anything is saved: nil
// (no cap) and 1 MB to 1 PB are fine. The panel's cap field asks it too.
func CheckVolumeCap(mb *int64) error {
	switch {
	case mb == nil:
		return nil
	case *mb > maxCapMB:
		return errs.Invalidf("max_size_mb", "Give a size up to 1 PB.")
	case *mb < 1:
		return errs.Invalidf("max_size_mb", "Give a size of at least 1 MB, or leave it empty for no cap.")
	}
	return nil
}

// SetVolumeCap sets a volume's size cap in MB, nil for none. On an xfs disk
// the folder's quota changes at once; a cap under what the volume holds is
// refused and nothing changes. On any other volume the cap is saved and not
// enforced (CapEnforced).
func (o *Orchestrator) SetVolumeCap(ctx context.Context, volumeID string, mb *int64) error {
	if err := CheckVolumeCap(mb); err != nil {
		return err
	}
	v, err := o.store.Volumes.Get(ctx, volumeID)
	if err != nil {
		return err
	}
	d, enforced, err := o.xfsDiskOf(ctx, v)
	if err != nil {
		return err
	}
	old := v.MaxSizeMB
	v.MaxSizeMB = mb
	if err := o.store.Volumes.Update(ctx, v); err != nil {
		return err
	}
	if !enforced {
		return nil
	}
	if err := o.diskLeaf.SetCap(ctx, d, v.Name); err != nil {
		if disk.Ambiguous(err) {
			// the host may have taken the new limit: the row keeps it (it is what the
			// next place puts on the folder) and the panel says it is not confirmed
			return fmt.Errorf("the cap is saved but the disk did not confirm it, it is applied on the next deploy: %w", err)
		}
		v.MaxSizeMB = old
		if rerr := o.store.Volumes.Update(context.WithoutCancel(ctx), v); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	return nil
}

// Volume is one volume by id.
func (o *Orchestrator) Volume(ctx context.Context, id string) (Volume, error) {
	return o.volumes.Get(ctx, id)
}

// TileVolumes are the env volumes a tile mounts.
func (o *Orchestrator) TileVolumes(ctx context.Context, tileID string) ([]Volume, error) {
	t, err := o.tiles.Get(ctx, tileID)
	if err != nil {
		return nil, err
	}
	vs, err := o.volumes.List(ctx, VolumeScope{Kind: "env", ID: t.EnvironmentID})
	if err != nil {
		return nil, err
	}
	var out []Volume
	for _, v := range vs {
		for _, l := range tile.Lines(t.Volumes) {
			if sl, _, ok := MountedVolume(l); ok && sl == v.Slug {
				out = append(out, v)
				break
			}
		}
	}
	return out, nil
}
