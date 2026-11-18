package service

import (
	"context"
	"errors"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
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

// DeleteVolume refuses while a tile mounts it (leaf/volume). Otherwise the
// volume is orphaned like a removed env volume: its data stays for the
// retention window, then the orphan job backs it up and removes it, so its
// backups keep their volume and stay restorable. Its schedules go now. On a
// forgotten node only the row goes: there is no Docker volume left to reach.
func (o *Orchestrator) DeleteVolume(ctx context.Context, id string) error {
	v, err := o.volumes.Get(ctx, id)
	if err != nil {
		return err
	}
	ts, err := o.mounters(ctx, v)
	if err != nil {
		return err
	}
	var by []string
	for _, t := range ts {
		by = append(by, t.Slug)
	}
	if gone, err := o.pmForgotten(ctx, o.volumeNode(ctx, v.ID)); err != nil {
		return err
	} else if gone {
		return o.volumes.DeleteRow(ctx, v, by)
	}
	if err := o.volumes.Retire(ctx, v, by); err != nil {
		return err
	}
	ss, err := o.backups.Schedules(ctx, v.ID)
	for _, s := range ss {
		err = errors.Join(err, o.deleteBackupSchedule(ctx, s.ID))
	}
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
