package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
)

// HostGrantsWaiting (server admin) is how many stacks have a job parked on
// elevated access: the admin rail's badge. One query over the waiting jobs.
func (o *Orchestrator) HostGrantsWaiting(ctx context.Context) (int, error) {
	waiting, err := o.jobRows.List(ctx, job.Waiting)
	if err != nil {
		return 0, err
	}
	stacks := map[string]bool{}
	for _, j := range waiting {
		var p struct {
			HostAccess *hostAccess `json:"host_access"`
		}
		if json.Unmarshal([]byte(j.Payload), &p) == nil && p.HostAccess != nil {
			stacks[p.HostAccess.Stack] = true
		}
	}
	n := 0
	for id := range stacks {
		if !o.stackGone(ctx, id) {
			n++
		}
	}
	return n, nil
}

// stackGone: the stack row is not there (a grant or job outlived it).
func (o *Orchestrator) stackGone(ctx context.Context, id string) bool {
	_, err := o.stacks.Get(ctx, id)
	return errors.Is(err, errs.ErrNotFound)
}

// cancelWaiting cancels the waiting jobs for which gone says so (best
// effort: one that moved on meanwhile is left as it is).
func (o *Orchestrator) cancelWaiting(ctx context.Context, gone func(Job) bool) {
	waiting, err := o.jobRows.List(ctx, job.Waiting)
	if err != nil {
		return
	}
	for _, j := range waiting {
		if gone(j) {
			_ = o.jobs.Cancel(ctx, j.ID)
		}
	}
}

// cancelWaitingFor cancels the waiting jobs whose lock set holds key (an
// env: "env:<id>").
func (o *Orchestrator) cancelWaitingFor(ctx context.Context, key string) {
	o.cancelWaiting(ctx, func(j Job) bool { return slices.Contains(j.LockSet, key) })
}

// cancelWaitingTile cancels the waiting tile-kind jobs of one tile; a parked
// promote or sync of its env also locks the tile but has no tile_id payload.
func (o *Orchestrator) cancelWaitingTile(ctx context.Context, id string) {
	o.cancelWaiting(ctx, func(j Job) bool {
		var p tileJob
		return slices.Contains(j.LockSet, id) && json.Unmarshal([]byte(j.Payload), &p) == nil && p.TileID == id
	})
}

// dropTileGrant removes a deleted or renamed tile's grant lines that no
// surviving tile of the same slug still asks for (the row is per stack, and
// PR envs never hold grants, so their tiles do not count).
func (o *Orchestrator) dropTileGrant(ctx context.Context, t Tile) error {
	e, err := o.envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return err
	}
	envs, err := o.envs.List(ctx, e.StackID)
	if err != nil {
		return err
	}
	var left []Tile
	for _, en := range envs {
		if en.Type == environment.Ephemeral {
			continue
		}
		ts, err := o.tiles.List(ctx, en.ID)
		if err != nil {
			return err
		}
		for _, x := range ts {
			if x.ID != t.ID && x.Slug == t.Slug {
				left = append(left, x)
			}
		}
	}
	changed, err := o.hostgrant.Retain(ctx, e.StackID, t.Slug, deploy.HostSet(left...).Lines)
	if err != nil {
		return err
	}
	if changed {
		o.rerouteVIPsAsync(ctx) // cut LAN access now, not at the next deploy
	}
	return nil
}

// dropRemoved runs the grant and waiting-job cleanup for the tiles a promote
// or sync plan removed (ids), looked up in pre, the env's rows from before.
func (o *Orchestrator) dropRemoved(ctx context.Context, pre []Tile, removed []string) error {
	var err error
	for _, t := range pre {
		if slices.Contains(removed, t.ID) {
			err = errors.Join(err, o.dropTileGrant(ctx, t))
			o.cancelWaitingTile(ctx, t.ID)
		}
	}
	return err
}
