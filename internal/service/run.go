package service

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

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

type ranFirstKey struct{}

// ranFirst is the on_deploy functions a dependent already ran in one deploy.
type ranFirst struct {
	mu    sync.Mutex
	ids   map[string]bool
	since time.Time
}

// withRanFirst marks ctx as one deploy's: runFirst records into it, afterDeploy skips what it holds.
// since is when the job was created: a resumed job has lost the marker, so
// a deploy run from after that counts as run too.
func withRanFirst(ctx context.Context, since time.Time) context.Context {
	return context.WithValue(ctx, ranFirstKey{}, &ranFirst{ids: map[string]bool{}, since: since})
}

func ranFirstOf(ctx context.Context) *ranFirst {
	r, _ := ctx.Value(ranFirstKey{}).(*ranFirst)
	return r
}

// runFirst is a `tile:completed` dependency on an on_deploy function: run it
// now, inside the dependent's job, and fail the dependent when the run does.
// A function this deploy already ran (for another dependent, or before a
// park) is not run again; one already going is waited for. No row is left
// open when the run stops short: no job carries it, so nothing else would
// close it.
func (o *Orchestrator) runFirst(ctx context.Context, tileID string, log io.Writer) error {
	t, err := o.tiles.Get(ctx, tileID)
	if err != nil {
		return err
	}
	if ran, err := o.ranThisDeployOrBefore(ctx, t); ran || err != nil {
		return err
	}
	if err := o.deploy.AwaitIdle(ctx, t, o.run.Busy, log); err != nil {
		return err
	}
	r, ok, err := o.run.Queue(ctx, tileID, lrun.Deploy)
	if err != nil {
		return err
	}
	if !ok {
		return errs.Conflictf("a run of this tile is still going; try again when it ends")
	}
	if err = o.run.Do(ctx, r.ID, log); err != nil {
		// Do parks on an unset param with the row still queued. A no-op for
		// a row it already closed.
		if _, ferr := o.runs.Finish(context.WithoutCancel(ctx), r.ID, nil, lrun.Cancelled, err.Error()); ferr != nil {
			err = errors.Join(err, ferr)
		}
		return err
	}
	if rf := ranFirstOf(ctx); rf != nil {
		rf.mu.Lock()
		rf.ids[tileID] = true
		rf.mu.Unlock()
	}
	return nil
}

// ranThisDeployOrBefore: the deploy's marker holds the function, or (a
// resumed job lost its marker) its last run is a deploy run that went ok on
// the env's current release after the job was created.
func (o *Orchestrator) ranThisDeployOrBefore(ctx context.Context, t Tile) (bool, error) {
	rf := ranFirstOf(ctx)
	if rf == nil {
		return false, nil
	}
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if rf.ids[t.ID] {
		return true, nil
	}
	if rf.since.IsZero() {
		return false, nil
	}
	e, err := o.envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return false, err
	}
	r, ok, err := o.runs.Last(ctx, t.ID)
	if err != nil || !ok {
		return false, err
	}
	same := (r.ReleaseID == nil) == (e.ReleaseID == nil) && (r.ReleaseID == nil || *r.ReleaseID == *e.ReleaseID)
	if r.Trigger == lrun.Deploy && r.Status == lrun.OK && same && !r.CreatedAt.Before(rf.since) {
		rf.ids[t.ID] = true
		return true, nil
	}
	return false, nil
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
		case t.Kind == tile.Function && t.Trigger == tile.OnDeploy && !ranThisDeploy(ctx, id):
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

func ranThisDeploy(ctx context.Context, id string) bool {
	rf := ranFirstOf(ctx)
	if rf == nil {
		return false
	}
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.ids[id]
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
	t, _, err = o.UpdateTile(ctx, id, func(t *Tile) error {
		t.Paused = paused
		return nil
	})
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
func (o *Orchestrator) FollowRunLog(
	ctx context.Context,
	tileID, runID string,
	tail int,
) (<-chan string, func(), error) {
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
