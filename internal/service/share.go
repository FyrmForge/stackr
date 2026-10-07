package service

import (
	"context"
	"slices"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	// Share is an org's NFS or SMB export; User and PasswordRef are
	// ${{ org.params }} refs, never values.
	Share     = store.Share
	ShareSpec = volume.ShareSpec
)

// Shares are the org's network shares, by slug.
func (o *Orchestrator) Shares(ctx context.Context, orgID string) ([]Share, error) {
	return o.volumes.Shares(ctx, orgID)
}

// CreateShare adds a share to the org (leaf/volume holds the rules).
func (o *Orchestrator) CreateShare(ctx context.Context, orgID string, sp ShareSpec) (Share, error) {
	return o.volumes.CreateShare(ctx, orgID, sp)
}

// DeleteShare removes one of the org's shares by slug or id; another org's
// is not found. Refused while a tile line mounts it.
func (o *Orchestrator) DeleteShare(ctx context.Context, orgID, ref string) error {
	s, err := o.volumes.ShareOf(ctx, orgID, ref)
	if err != nil {
		return err
	}
	users, err := o.shareUsers(ctx, orgID)
	if err != nil {
		return err
	}
	return o.volumes.DeleteShare(ctx, s, users[s.Slug])
}

// UpdateShare rewrites one of the org's shares by slug or id, then drops the
// volumes of its old recipe that no tile row names any more.
func (o *Orchestrator) UpdateShare(ctx context.Context, orgID, ref string, sp ShareSpec) (Share, error) {
	s, err := o.volumes.ShareOf(ctx, orgID, ref)
	if err != nil {
		return Share{}, err
	}
	if s, err = o.volumes.UpdateShare(ctx, s, sp); err != nil {
		return s, err
	}
	uses := []volume.ShareUse{}
	err = o.eachShareLine(ctx, orgID, func(_ store.Tile, m tile.Mount) {
		uses = append(uses, volume.ShareUse{Share: m.Share, Sub: m.Sub})
	})
	if err != nil {
		return s, err
	}
	// ponytail: volumes a container still holds stay until the next sweep
	// (a tile removal); a held one is not an error here.
	_, err = o.volumes.SweepShares(ctx, orgID, uses)
	return s, err
}

// shareUsers maps each share slug to the slugs of the org's tiles whose
// volumes lines mount it.
func (o *Orchestrator) shareUsers(ctx context.Context, orgID string) (map[string][]string, error) {
	out := map[string][]string{}
	err := o.eachShareLine(ctx, orgID, func(t store.Tile, m tile.Mount) {
		if !slices.Contains(out[m.Share], t.Slug) {
			out[m.Share] = append(out[m.Share], t.Slug)
		}
	})
	return out, err
}

// eachShareLine calls fn for every share line of every tile row in the org.
func (o *Orchestrator) eachShareLine(ctx context.Context, orgID string, fn func(store.Tile, tile.Mount)) error {
	sts, err := o.stacks.List(ctx, orgID)
	if err != nil {
		return err
	}
	for _, st := range sts {
		ts, err := o.tiles.ListByStack(ctx, st.ID)
		if err != nil {
			return err
		}
		for _, t := range ts {
			for _, l := range tile.Lines(t.Volumes) {
				if m, err := tile.ParseMount(l); err == nil && m.Kind == tile.MountShare {
					fn(t, m)
				}
			}
		}
	}
	return nil
}
