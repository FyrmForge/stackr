package service

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// EnvVolume is one volume the env declares, as its Settings row lists it.
type EnvVolume struct {
	Volume
	SizeBytes int64    // -1 = Docker cannot tell (never deployed, node away)
	MountedBy []string // the tiles whose volumes lines name it
	OwnedBy   string   // the managed tile that owns it; "" = the env's own
}

// EnvVolumes is the env's volumes. Managed: a stack file declares them, so
// the panel only reads.
type EnvVolumes struct {
	Volumes []EnvVolume
	Managed bool
}

// ListEnvVolumes is the env's live volumes (orphans are gone from it).
func (o *Orchestrator) ListEnvVolumes(ctx context.Context, envID string) (EnvVolumes, error) {
	var out EnvVolumes
	e, err := o.envs.Get(ctx, envID)
	if err != nil {
		return out, err
	}
	st, err := o.stacks.Get(ctx, e.StackID)
	if err != nil {
		return out, err
	}
	out.Managed = st.ConfigRepo != ""
	vs, err := o.volumes.List(ctx, VolumeScope{Kind: "env", ID: envID})
	if err != nil {
		return out, err
	}
	ts, err := o.tiles.List(ctx, envID)
	if err != nil {
		return out, err
	}
	owners := map[string]string{}
	for _, t := range ts {
		if t.Kind != tile.Managed {
			continue
		}
		m, err := o.managed.GetByTile(ctx, t.ID)
		if errors.Is(err, errs.ErrNotFound) {
			continue
		}
		if err != nil {
			return out, err
		}
		owners[m.ID] = t.Slug
	}
	for _, v := range vs {
		if v.OrphanedAt != nil {
			continue
		}
		ev := EnvVolume{Volume: v, SizeBytes: o.volumes.SizeBytes(ctx, v), MountedBy: mountedBy(ts, v.Slug)}
		if v.InstanceID != nil {
			ev.OwnedBy = cmp.Or(owners[*v.InstanceID], "a managed instance")
		}
		out.Volumes = append(out.Volumes, ev)
	}
	slices.SortFunc(out.Volumes, func(a, b EnvVolume) int { return strings.Compare(a.Slug, b.Slug) })
	return out, nil
}

// DeclareEnvVolume adds a volume to the env. An orphan of the same name is
// re-adopted, data and all (adopted says so); a live one is refused, so an
// instance's volume is never taken over.
func (o *Orchestrator) DeclareEnvVolume(ctx context.Context, envID, name string) (v Volume, adopted bool, err error) {
	if err := o.envVolumesWritable(ctx, envID); err != nil {
		return v, false, err
	}
	sc := VolumeScope{Kind: "env", ID: envID}
	name = strings.TrimSpace(name)
	if cur, err := o.volumes.BySlug(ctx, sc, name); err == nil && cur.OrphanedAt == nil {
		return cur, false, errs.Invalidf("volume", "This environment already has a volume named %s.", name)
	} else if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return v, false, err
	}
	return o.volumes.Declare(ctx, sc, name, nil)
}

// RemoveEnvVolume orphans a volume of the env: its data stays for the
// retention window, and declaring the name again takes it back. Refused
// while a tile mounts it or a managed instance owns it.
func (o *Orchestrator) RemoveEnvVolume(ctx context.Context, envID, volumeID string) error {
	if err := o.envVolumesWritable(ctx, envID); err != nil {
		return err
	}
	v, err := o.volumes.Get(ctx, volumeID)
	if err != nil {
		return err
	}
	if v.ScopeKind != "env" || v.ScopeID != envID || v.OrphanedAt != nil {
		return errs.ErrNotFound
	}
	if v.InstanceID != nil {
		return errs.Conflictf("%s belongs to a managed instance; it goes when the instance does", v.Slug)
	}
	ts, err := o.tiles.List(ctx, envID)
	if err != nil {
		return err
	}
	if by := mountedBy(ts, v.Slug); len(by) > 0 {
		return errs.Conflictf("%s is mounted by %s; take it out of their Storage first", v.Slug, strings.Join(by, ", "))
	}
	_, err = o.volumes.Orphan(ctx, v)
	return err
}

// envVolumesWritable refuses a config-managed stack: its file declares
// the env's volumes, and a sync would orphan one added here.
func (o *Orchestrator) envVolumesWritable(ctx context.Context, envID string) error {
	e, err := o.envs.Get(ctx, envID)
	if err != nil {
		return err
	}
	st, err := o.stacks.Get(ctx, e.StackID)
	if err != nil {
		return err
	}
	if st.ConfigRepo != "" {
		return errs.Conflictf("stack %s is config-managed; declare volumes in its stack file", st.Slug)
	}
	return nil
}

// checkTileVolumes refuses a volumes line naming a volume the env does not
// declare (or has orphaned): deploy would refuse it later, less helpfully.
func (o *Orchestrator) checkTileVolumes(ctx context.Context, t Tile) error {
	vs, err := o.volumes.List(ctx, VolumeScope{Kind: "env", ID: t.EnvironmentID})
	if err != nil {
		return err
	}
	var missing []string
	for _, l := range tile.Lines(t.Volumes) {
		sl, _, ok := MountedVolume(l)
		if !ok || slices.Contains(missing, sl) {
			continue
		}
		if !slices.ContainsFunc(vs, func(v Volume) bool { return v.Slug == sl && v.OrphanedAt == nil }) {
			missing = append(missing, sl)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return errs.Invalidf("volumes", "This environment does not declare %s. Declare it in env Settings first.", strings.Join(missing, ", "))
}

// mountedBy are the slugs of the tiles in ts whose volumes lines name sl.
func mountedBy(ts []Tile, sl string) []string {
	var out []string
	for _, t := range ts {
		for _, l := range tile.Lines(t.Volumes) {
			if s, _, ok := MountedVolume(l); ok && s == sl {
				out = append(out, t.Slug)
				break
			}
		}
	}
	return out
}


// MountableVolumes are the names a tile of the env may mount: its live
// volumes a managed instance does not own.
func (o *Orchestrator) MountableVolumes(ctx context.Context, envID string) ([]string, error) {
	vs, err := o.volumes.List(ctx, VolumeScope{Kind: "env", ID: envID})
	var out []string
	for _, v := range vs {
		if v.OrphanedAt == nil && v.InstanceID == nil {
			out = append(out, v.Slug)
		}
	}
	slices.Sort(out)
	return out, err
}
