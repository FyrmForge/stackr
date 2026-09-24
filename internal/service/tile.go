package service

import (
	"context"
	"io"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/imagewatch"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Image = store.Image

// TileStatus is the box's word for a tile plus its newest job (DECIDE 8).
type TileStatus struct {
	Word     string      `json:"word"` // running | stopped | partial | … (leaf/tile)
	Replicas []Container `json:"replicas"`
	LastJob  *Job        `json:"last_job"`
}

func (o *Orchestrator) Tiles(ctx context.Context, envID string) ([]Tile, error) {
	return o.tiles.List(ctx, envID)
}

func (o *Orchestrator) TileStatus(ctx context.Context, id string) (TileStatus, error) {
	t, err := o.tiles.Get(ctx, id)
	if err != nil {
		return TileStatus{}, err
	}
	s, err := o.tiles.State(ctx, t)
	if err != nil {
		return TileStatus{}, err
	}
	out := TileStatus{Word: s.Word, Replicas: s.Replicas}
	if j, ok, err := o.jobRows.Last(ctx, id); err != nil {
		return out, err
	} else if ok {
		out.LastJob = &j
	}
	return out, nil
}

// CreateTile is the one tile-create path for image and service tiles (B26);
// managed tiles go through CreateManagedTile. Nothing runs until Deploy.
func (o *Orchestrator) CreateTile(ctx context.Context, t Tile) (Tile, error) {
	if t.Kind == tile.Managed {
		return Tile{}, errs.Invalidf("kind", "Create a managed tile with its engine.")
	}
	return o.tiles.Create(ctx, t)
}

// UpdateTile reads the row, applies edit, and writes the whole row back, so
// a field the caller did not touch is never blanked (B1, B21). What the
// write earns runs: a proxy push, a redeploy job when the tile runs.
func (o *Orchestrator) UpdateTile(ctx context.Context, id string, edit func(*Tile) error) (Tile, *Job, error) {
	old, err := o.tiles.Get(ctx, id)
	if err != nil {
		return old, nil, err
	}
	cur := old
	if err := edit(&cur); err != nil {
		return old, nil, err
	}
	t, effects, err := o.tiles.Update(ctx, old, cur)
	if err != nil {
		return t, nil, err
	}
	var j *Job
	for _, e := range effects {
		switch e {
		case tile.Route:
			err = o.sync.Sync(ctx)
		case tile.Redeploy:
			j, err = o.redeployIfRunning(ctx, t)
		case tile.CronReload:
			o.sched.Reload(ctx)
		}
		if err != nil {
			return t, j, err
		}
	}
	if old.Kind == tile.Cron && t.Kind != tile.Cron {
		o.sched.Reload(ctx) // its entry goes
	}
	return t, j, nil
}

func (o *Orchestrator) RenameTile(ctx context.Context, id, name string) (Tile, error) {
	t, err := o.tiles.Get(ctx, id)
	if err != nil {
		return t, err
	}
	return o.tiles.Rename(ctx, t, name)
}

// DeleteTile queues the tile's removal: containers, ingress, slices, row.
func (o *Orchestrator) DeleteTile(ctx context.Context, id string) (Job, error) {
	return o.enqueue(ctx, kindDelete, tileJob{TileID: id}, id)
}

// Deploy queues a (re)deploy on the image the env's release pins (B34); a
// newer deploy of the tile supersedes a queued one. One gate for deploy and
// redeploy (B29).
func (o *Orchestrator) Deploy(ctx context.Context, id string) (Job, error) {
	return o.enqueue(ctx, kindDeploy, tileJob{TileID: id}, id)
}

func (o *Orchestrator) RestartTile(ctx context.Context, id string) (Job, error) {
	return o.enqueue(ctx, kindRestart, tileJob{TileID: id}, id)
}

func (o *Orchestrator) StopTile(ctx context.Context, id string) (Job, error) {
	return o.enqueue(ctx, kindStop, tileJob{TileID: id}, id)
}

func (o *Orchestrator) StartTile(ctx context.Context, id string) (Job, error) {
	return o.enqueue(ctx, kindStart, tileJob{TileID: id}, id)
}

// Logs is the tail of one replica's log; FollowLogs streams it.
func (o *Orchestrator) Logs(ctx context.Context, tileID, containerID string, tail int) (string, error) {
	return o.tiles.Logs(ctx, tileID, containerID, tail)
}

func (o *Orchestrator) FollowLogs(ctx context.Context, tileID, containerID string, tail int) (<-chan string, func(), error) {
	return o.tiles.Follow(ctx, tileID, containerID, tail)
}

// Terminal is an interactive exec in one replica.
func (o *Orchestrator) Terminal(ctx context.Context, tileID, containerID string, cmd []string, stdin io.Reader) (io.Reader, func() error, error) {
	return o.tiles.Terminal(ctx, tileID, containerID, cmd, stdin)
}

// CheckImages queues an image-watch check of one stack or tile ("" = all).
func (o *Orchestrator) CheckImages(ctx context.Context, stackID, tileID string) (Job, error) {
	return o.enqueue(ctx, kindImageWatch, imagewatch.Scope{StackID: stackID, TileID: tileID},
		"imagewatch", "imagewatch:"+stackID+"/"+tileID)
}

// Images is every image row with its watch cache.
func (o *Orchestrator) Images(ctx context.Context) ([]Image, error) { return o.images.List(ctx) }

// redeployIfRunning queues a deploy for a tile with replicas; nil job = not running.
func (o *Orchestrator) redeployIfRunning(ctx context.Context, t Tile) (*Job, error) {
	cs, err := o.tiles.Replicas(ctx, t)
	if err != nil || len(cs) == 0 {
		return nil, err
	}
	j, err := o.Deploy(ctx, t.ID)
	return &j, err
}

// redeployRunning: a param or settings change reaches every running tile
// it may touch, on the image its env's release pins (B34).
func (o *Orchestrator) redeployRunning(ctx context.Context, ts []Tile) error {
	for _, t := range ts {
		if _, err := o.redeployIfRunning(ctx, t); err != nil {
			return err
		}
	}
	return nil
}
