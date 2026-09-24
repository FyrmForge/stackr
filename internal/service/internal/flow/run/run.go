// Package run is one run of a cron or function tile: the run row first
// (Queue), then, inside the job the orchestrator enqueues for it, the
// container (Do): built by flow/deploy's spec builder (the run -> deploy
// edge), started, waited on, removed, the row closed with the exit code.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	lrun "github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Flow struct {
	Tiles  *tile.Leaf
	Envs   *environment.Leaf
	Runs   *lrun.Leaf
	Jobs   *job.Leaf
	Deploy *deploy.Flow
	// Minute is one timeout_minutes unit; tests shorten it. 0 = a minute.
	Minute time.Duration
}

// Overlap is the reason on a run refused because the tile's last one is
// still queued or running.
const Overlap = "previous run still going"

// Queue writes the run row for a run of tileID. ok=false: a run of the
// tile is still going, so the row is written cancelled and nothing more
// happens; otherwise the caller enqueues the job and records it (SetJob).
func (f *Flow) Queue(ctx context.Context, tileID, trigger string) (r store.Run, ok bool, err error) {
	t, err := f.Tiles.Get(ctx, tileID)
	if err != nil {
		return r, false, err
	}
	if !tile.RunToCompletion(t.Kind) {
		return r, false, errs.Invalidf("kind", "run applies to cron and function tiles")
	}
	e, err := f.Envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return r, false, err
	}
	busy, err := f.busy(ctx, tileID)
	if err != nil {
		return r, false, err
	}
	if r, err = f.Runs.Start(ctx, tileID, e.ReleaseID, trigger); err != nil || !busy {
		return r, err == nil, err
	}
	r, err = f.Runs.Finish(ctx, r.ID, nil, lrun.Cancelled, Overlap)
	return r, false, err
}

// busy: the tile has a run still going. A queued or running row whose job
// already ended (cancelled while queued, lost) is closed here instead, so
// one lost job never blocks the tile's runs for good.
func (f *Flow) busy(ctx context.Context, tileID string) (bool, error) {
	a, ok, err := f.Runs.Active(ctx, tileID)
	if err != nil || !ok || a.JobID == "" {
		return ok, err
	}
	j, err := f.Jobs.Get(ctx, a.JobID)
	if errors.Is(err, errs.ErrNotFound) || (err == nil && job.Terminal(j.State)) {
		_, err = f.Runs.Finish(ctx, a.ID, nil, lrun.Cancelled, "its job ended before the run did")
		return false, err
	}
	return err == nil, err
}

// Do is the job body for run runID: build the spec on the image the env's
// release pins, start the container, wait (up to the tile's timeout), close
// the row. The job fails when the run does. An unset param parks the job
// with the row still queued.
func (f *Flow) Do(ctx context.Context, runID string, log io.Writer) error {
	r, err := f.Runs.Get(ctx, "", runID)
	if errors.Is(err, errs.ErrNotFound) {
		return nil // pruned or the tile is gone: nothing to run
	}
	if err != nil || lrun.Done(r) {
		return err
	}
	fail := func(status, reason string, exit *int) error {
		if ctx.Err() != nil {
			status, reason = lrun.Cancelled, "stopped"
		}
		_, ferr := f.Runs.Finish(context.WithoutCancel(ctx), r.ID, exit, status, reason)
		return errors.Join(errors.New(reason), ferr)
	}
	t, err := f.Tiles.Get(ctx, r.TileID)
	if err != nil {
		return fail(lrun.Failed, err.Error(), nil)
	}
	e, err := f.Envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return fail(lrun.Failed, err.Error(), nil)
	}
	ref, _, err := f.Deploy.Current(ctx, t, e)
	if err != nil {
		return fail(lrun.Failed, err.Error(), nil)
	}
	spec, err := f.Deploy.Spec(ctx, t, ref, log)
	var unset errs.Unset
	if errors.As(err, &unset) {
		return err
	}
	if err != nil {
		return fail(lrun.Failed, err.Error(), nil)
	}
	if r, err = f.Runs.Begin(ctx, r.ID); err != nil || lrun.Done(r) {
		return err
	}
	out, err := f.Runs.OpenLog(r)
	if err != nil {
		return fail(lrun.Failed, err.Error(), nil)
	}
	defer func() { _ = out.Close() }()

	rctx, cancel := ctx, func() {}
	if t.TimeoutMinutes > 0 {
		unit := f.Minute
		if unit == 0 {
			unit = time.Minute
		}
		rctx, cancel = context.WithTimeout(ctx, time.Duration(t.TimeoutMinutes)*unit)
	}
	defer cancel()
	_, _ = fmt.Fprintf(log, "running %s on %s\n", t.Slug, ref)
	code, err := f.Tiles.RunOnce(rctx, t, spec, r.ID, out)
	switch {
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return fail(lrun.Failed, fmt.Sprintf("timed out after %d minutes", t.TimeoutMinutes), nil)
	case ctx.Err() != nil:
		return fail(lrun.Cancelled, "stopped", nil)
	case err != nil:
		return fail(lrun.Failed, err.Error(), nil)
	case code != 0:
		return fail(lrun.Failed, fmt.Sprintf("exited with code %d", code), &code)
	}
	_, err = f.Runs.Finish(ctx, r.ID, &code, lrun.OK, "")
	_, _ = fmt.Fprintf(log, "%s exited 0\n", t.Slug)
	return err
}
