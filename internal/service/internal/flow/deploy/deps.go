package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	lrun "github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	depPoll    = 2 * time.Second
	depCap     = 10 * time.Minute
	depDefault = 30 * time.Second // docker's healthcheck interval when none is set
	depRetries = 3                // and its retries
)

// awaitDeps holds t's rollout until every dependency meets its condition:
// db:healthy until its replicas report healthy, init:completed until the
// one-shot or function tile's last run exited 0 (an on_deploy function is
// run first). A bare dependency is only an ordering and never waits. A
// dependency that is no tile of this env (a shared source) is skipped.
func (f *Flow) awaitDeps(ctx context.Context, t store.Tile, log io.Writer) error {
	for _, l := range tile.Lines(t.DependsOn) {
		s, cond, err := tile.ParseDep(l)
		if err != nil || cond == "started" {
			continue
		}
		dt, err := f.Tiles.GetBySlug(ctx, t.EnvironmentID, s)
		if errors.Is(err, errs.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if cond == "healthy" {
			err = f.awaitHealthy(ctx, dt, log)
		} else {
			err = f.awaitCompleted(ctx, dt, log)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Waits reports whether t holds its rollout for a dependency's health or
// completion (a bare dependency is only an ordering).
func Waits(t store.Tile) bool {
	return slices.ContainsFunc(tile.Lines(t.DependsOn), func(l string) bool {
		_, cond, err := tile.ParseDep(l)
		return err == nil && cond != "started"
	})
}

// poll runs ready every depPoll until it says yes, the timeout passes, or
// ctx ends. The timeout is counted in polls, so a test never waits.
func (f *Flow) poll(ctx context.Context, timeout time.Duration, ready func() (bool, error)) (bool, error) {
	sleep := f.DepSleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			select {
			case <-time.After(d):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	for waited := time.Duration(0); ; waited += depPoll {
		ok, err := ready()
		if ok || err != nil {
			return ok, err
		}
		if waited >= timeout {
			return false, nil
		}
		if err := sleep(ctx, depPoll); err != nil {
			return false, err
		}
	}
}

// healthTimeout is the dependency's start period plus retries times
// interval, at most depCap. A value the tile does not set comes from the
// image's own HEALTHCHECK (read off a replica), then docker's defaults.
func (f *Flow) healthTimeout(ctx context.Context, d store.Tile) time.Duration {
	interval := time.Duration(d.HealthcheckIntervalS) * time.Second
	start := time.Duration(d.HealthcheckStartPeriodS) * time.Second
	retries := d.HealthcheckRetries
	if interval == 0 || start == 0 || retries == 0 {
		img := f.imageHealth(ctx, d)
		if interval == 0 {
			interval = img.HealthInterval
		}
		if start == 0 {
			start = img.HealthStartPeriod
		}
		if retries == 0 {
			retries = img.HealthRetries
		}
	}
	if interval == 0 {
		interval = depDefault
	}
	if retries == 0 {
		retries = depRetries
	}
	return min(start+time.Duration(retries)*interval, depCap)
}

// imageHealth is the effective healthcheck of one live replica of d; zero
// when there is none to read.
func (f *Flow) imageHealth(ctx context.Context, d store.Tile) docker.Detail {
	cs, err := f.Tiles.Replicas(ctx, d)
	if err != nil {
		return docker.Detail{}
	}
	for _, c := range cs {
		if c.State == "running" {
			det, _ := f.Tiles.Inspect(ctx, c.ID)
			return det
		}
	}
	return docker.Detail{}
}

// queuedBehind fails when a job of d is queued: it cannot start while this
// deploy holds the worker or the lock, so waiting for d would end only at
// the timeout.
func (f *Flow) queuedBehind(ctx context.Context, d store.Tile) error {
	if f.Jobs == nil {
		return nil
	}
	js, err := f.Jobs.History(ctx, []string{d.ID}, 20)
	if err != nil {
		return err
	}
	if i := slices.IndexFunc(js, func(j store.Job) bool { return j.State == job.Queued }); i >= 0 {
		return fmt.Errorf("%s has a %s queued behind this deploy; let it finish and promote again", d.Slug, js[i].Kind)
	}
	return nil
}

// AwaitIdle waits for d's run that is going to end. busy is flow/run's Busy
// (it closes a run whose job was lost); a queued job of d fails the wait.
func (f *Flow) AwaitIdle(
	ctx context.Context,
	d store.Tile,
	busy func(context.Context, string) (bool, error),
	log io.Writer,
) error {
	idle := func() (bool, error) {
		b, err := busy(ctx, d.ID)
		if b && err == nil {
			err = f.queuedBehind(ctx, d)
		}
		return !b, err
	}
	if ok, err := idle(); ok || err != nil {
		return err
	}
	logf(log, "waiting for the run of %s to end\n", d.Slug)
	ok, err := f.poll(ctx, depCap, idle)
	if err == nil && !ok {
		err = fmt.Errorf("waited for the run of %s to end; it did not", d.Slug)
	}
	return err
}

func (f *Flow) awaitHealthy(ctx context.Context, d store.Tile, log io.Writer) error {
	healthy := func() (bool, error) {
		cs, err := f.Tiles.Replicas(ctx, d)
		if err != nil {
			return false, err
		}
		// An exited leftover is no replica: it neither fails nor passes.
		cs = slices.DeleteFunc(slices.Clone(cs), func(c docker.Container) bool {
			return c.State == "exited" || c.State == "dead"
		})
		if len(cs) == 0 {
			return false, fmt.Errorf("%s has nothing running", d.Slug)
		}
		for _, c := range cs {
			// No HEALTHCHECK at all: running is as healthy as it gets, the
			// same reading as docker.Healthy.
			if c.State != "running" || (c.Health != "" && c.Health != "healthy") {
				return false, f.queuedBehind(ctx, d)
			}
		}
		return true, nil
	}
	if ok, err := healthy(); ok || err != nil {
		return err
	}
	logf(log, "waiting for %s to be healthy\n", d.Slug)
	ok, err := f.poll(ctx, f.healthTimeout(ctx, d), healthy)
	if err == nil && !ok {
		err = fmt.Errorf("waited for %s to be healthy; it is not", d.Slug)
	}
	return err
}

func (f *Flow) awaitCompleted(ctx context.Context, d store.Tile, log io.Writer) error {
	if d.Kind == tile.Function && d.Trigger == tile.OnDeploy && f.RunFirst != nil {
		logf(log, "running %s first\n", d.Slug)
		return f.RunFirst(ctx, d.ID, log)
	}
	if f.Runs == nil {
		return nil
	}
	// Wait only for a run that is going; a last run that failed, or none at
	// all, will not change by waiting.
	notDone := fmt.Errorf("waited for %s to complete; it did not", d.Slug)
	done := func() (bool, error) {
		if _, active, err := f.Runs.Active(ctx, d.ID); active || err != nil {
			if err == nil {
				err = f.queuedBehind(ctx, d)
			}
			return false, err
		}
		r, ok, err := f.Runs.Last(ctx, d.ID)
		if err != nil {
			return false, err
		}
		if !ok || r.Status != lrun.OK {
			return false, notDone
		}
		return true, nil
	}
	if ok, err := done(); ok || err != nil {
		return err
	}
	logf(log, "waiting for %s to complete\n", d.Slug)
	ok, err := f.poll(ctx, depCap, done)
	if err == nil && !ok {
		err = notDone
	}
	return err
}
