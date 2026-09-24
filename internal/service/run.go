package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	lrun "github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Run = store.Run

// queueRun writes the run row and enqueues its job. The lock set holds the
// tile (a run waits for a deploy of it and the other way round) plus its
// own key, so one run never supersedes another. A run refused because the
// tile's last one is still going comes back cancelled with no job.
func (o *Orchestrator) queueRun(ctx context.Context, tileID, trigger string) (Job, Run, error) {
	r, ok, err := o.run.Queue(ctx, tileID, trigger)
	if err != nil || !ok {
		return Job{}, r, err
	}
	j, err := o.enqueue(ctx, kindRun, runJob{TileID: tileID, RunID: r.ID}, tileID, "run:"+r.ID)
	if err != nil {
		_, ferr := o.runs.Finish(context.WithoutCancel(ctx), r.ID, nil, lrun.Failed, err.Error())
		if ferr != nil {
			return j, r, ferr
		}
		return j, r, err
	}
	r, err = o.runs.SetJob(ctx, r.ID, j.ID)
	return j, r, err
}

// afterDeploy is what a deploy or promote of these tiles ends with: a cron
// reloads the schedule table (reload: always, a promote may have dropped or
// paused one), an on_deploy function queues one run.
func (o *Orchestrator) afterDeploy(ctx context.Context, tileIDs []string, reload bool) error {
	for _, id := range tileIDs {
		t, err := o.tiles.Get(ctx, id)
		if err != nil {
			return err
		}
		switch {
		case t.Kind == tile.Cron:
			reload = true
		case t.Kind == tile.Function && t.Trigger == tile.OnDeploy:
			if _, _, err := o.queueRun(ctx, id, lrun.Deploy); err != nil {
				return err
			}
		}
	}
	if reload {
		o.sched.Reload(ctx)
	}
	return nil
}

// deployedCrons is every cron tile its env's release pins: the schedule
// table's cron rows. A cron never deployed has no image to run.
func (o *Orchestrator) deployedCrons(ctx context.Context) ([]Tile, error) {
	ts, err := o.tiles.ListByKind(ctx, tile.Cron)
	if err != nil {
		return nil, err
	}
	pins := map[string]map[string]release.Pin{}
	var out []Tile
	for _, t := range ts {
		e, err := o.envs.Get(ctx, t.EnvironmentID)
		if err != nil {
			return nil, err
		}
		if e.ReleaseID == nil {
			continue
		}
		p, ok := pins[*e.ReleaseID]
		if !ok {
			if p, err = o.releases.Pins(ctx, *e.ReleaseID); err != nil {
				return nil, err
			}
			pins[*e.ReleaseID] = p
		}
		if _, ok := p[t.Slug]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// dropRuns removes the run logs of deleted tiles; the rows went with the
// tile (cascade). A tile that never ran has no directory: a no-op.
func (o *Orchestrator) dropRuns(tileIDs []string) error {
	for _, id := range tileIDs {
		if err := o.runs.DropTile(id); err != nil {
			return err
		}
	}
	return nil
}

func ids(ts []Tile) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}
