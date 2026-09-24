package service

import (
	"context"
	"strings"

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
func (o *Orchestrator) DeclareVolume(ctx context.Context, s VolumeScope, slug string, maxSizeMB int) (Volume, error) {
	v, _, err := o.volumes.Declare(ctx, s, slug, maxSizeMB, nil)
	return v, err
}

// DeleteVolume refuses while a tile mounts it (leaf/volume).
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
	return o.volumes.Delete(ctx, v, by)
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
			if strings.HasPrefix(l, v.Slug+":") {
				out = append(out, t)
				break
			}
		}
	}
	return out, err
}
