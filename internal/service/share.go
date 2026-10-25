package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
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
// is not found. Refused while a tile line mounts it. The volumes on the
// nodes go in a reconcile job, so a node that is down is cleaned later.
func (o *Orchestrator) DeleteShare(ctx context.Context, orgID, ref string) error {
	s, err := o.volumes.ShareOf(ctx, orgID, ref)
	if err != nil {
		return err
	}
	users, err := o.shareUsers(ctx, orgID)
	if err != nil {
		return err
	}
	if err := o.volumes.DeleteShare(ctx, s, users[s.Slug]); err != nil {
		return err
	}
	o.reconcileShares(ctx)
	return nil
}

// UpdateShare rewrites one of the org's shares by slug or id, then queues the
// drop of the volumes of its old recipe that no tile row names any more.
func (o *Orchestrator) UpdateShare(ctx context.Context, orgID, ref string, sp ShareSpec) (Share, error) {
	s, err := o.volumes.ShareOf(ctx, orgID, ref)
	if err != nil {
		return Share{}, err
	}
	if s, err = o.volumes.UpdateShare(ctx, s, sp); err != nil {
		return s, err
	}
	o.reconcileShares(ctx)
	return s, nil
}

// reconcileShares queues the share volume reconcile across the nodes.
func (o *Orchestrator) reconcileShares(ctx context.Context) {
	if _, err := o.enqueue(ctx, kindShareReconcile, nil, "share-reconcile"); err != nil {
		slog.Warn("shares: reconcile not queued", "err", err)
	}
}

// runShareReconcile removes share volumes no row claims from every node that
// is up. An offline node is skipped: it is reconciled when it reconnects.
func (o *Orchestrator) runShareReconcile(ctx context.Context, r *jobs.Run) error {
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return err
	}
	uses := map[string][]volume.ShareUse{}
	for _, org := range orgs {
		err := o.eachShareLine(ctx, org.ID, func(_ store.Tile, m tile.Mount) {
			uses[org.ID] = append(uses[org.ID], volume.ShareUse{Share: m.Share, Sub: m.Sub})
		})
		if err != nil {
			return err
		}
	}
	return o.eachNode(ctx, func(ctx context.Context, n store.Node) error {
		if n.Status == node.StatusOffline {
			return nil
		}
		held, err := o.volumes.ReconcileShares(ctx, uses)
		if len(held) > 0 {
			_, _ = fmt.Fprintf(r.Log, "%s: %d share volumes stay: a container holds them\n", n.Name, len(held))
			err = errors.Join(err, fmt.Errorf("%d share volumes held, retrying", len(held)))
		}
		return err
	})
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
