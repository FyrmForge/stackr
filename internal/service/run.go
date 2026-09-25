package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/errs"
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

// RunTile queues a manual run of a cron or function. The run row exists
// before this returns; a run refused because the last one is still going
// comes back cancelled with a zero job.
func (o *Orchestrator) RunTile(ctx context.Context, id string) (Job, Run, error) {
	return o.queueRun(ctx, id, lrun.Manual)
}

// PauseTile turns a cron's schedule off (paused) or back on; the schedule
// table reloads through UpdateTile's CronReload effect.
func (o *Orchestrator) PauseTile(ctx context.Context, id string, paused bool) (Tile, error) {
	t, err := o.tiles.Get(ctx, id)
	if err != nil {
		return t, err
	}
	if t.Kind != tile.Cron {
		return t, errs.Invalidf("paused", "only cron tiles have a schedule to pause")
	}
	t, _, err = o.UpdateTile(ctx, id, func(t *Tile) error { t.Paused = paused; return nil })
	return t, err
}

// Runs is the tile's kept runs, newest first; limit ≤ 0 = all.
func (o *Orchestrator) Runs(ctx context.Context, tileID string, limit int) ([]Run, error) {
	return o.runs.List(ctx, tileID, limit)
}

// Run is one run of the tile; another tile's run is not found.
func (o *Orchestrator) Run(ctx context.Context, tileID, runID string) (Run, error) {
	return o.runs.Get(ctx, tileID, runID)
}

// StopRun cancels the run's job and closes the row as stopped. Ownership
// is checked first; a run already finished is not an error.
func (o *Orchestrator) StopRun(ctx context.Context, tileID, runID string) error {
	r, err := o.runs.Get(ctx, tileID, runID)
	if err != nil || lrun.Done(r) {
		return err
	}
	if r.JobID != "" {
		if err := o.jobs.Cancel(ctx, r.JobID); err != nil {
			return err
		}
	}
	_, err = o.runs.Finish(ctx, r.ID, nil, lrun.Cancelled, "stopped")
	return err
}

// RunLog is the tail of one run's log (the file, never the container).
func (o *Orchestrator) RunLog(ctx context.Context, tileID, runID string, tail int) (string, error) {
	r, err := o.runs.Get(ctx, tileID, runID)
	if err != nil {
		return "", err
	}
	return o.runs.Tail(r, tail)
}

// FollowRunLog streams one run's log until the run closes.
func (o *Orchestrator) FollowRunLog(ctx context.Context, tileID, runID string, tail int) (<-chan string, func(), error) {
	r, err := o.runs.Get(ctx, tileID, runID)
	if err != nil {
		return nil, nil, err
	}
	lines, stop := o.runs.Follow(r, tail, func() bool {
		cur, err := o.runs.Get(ctx, tileID, runID)
		return err != nil || lrun.Done(cur)
	})
	return lines, stop, nil
}
