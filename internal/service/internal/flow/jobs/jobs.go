// Package jobs is the durable queue over the jobs table. It imports no
// other flow and no flow imports it (DECIDE 12): the handlers are the flow
// functions service.New injects, and the orchestrator is the only thing that
// enqueues. Jobs run flows with no user.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Kind names what a job does; each has one Handler.
type Kind string

// Handler runs one job. It returns errs.Unset to park the job on a param.
type Handler func(ctx context.Context, r *Run) error

// Run is what a handler gets: the row, its log, and the swap signal.
type Run struct {
	Job store.Job
	Log io.Writer
	a   *active
	r   *Runner
}

// Swap marks the start of the swap phase: from here on a newer job waits
// for this one instead of cancelling it. It refuses when a newer job has
// already superseded this one; the handler must stop then.
func (r *Run) Swap() error {
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	if r.a.superseded {
		return ErrSuperseded
	}
	r.a.swapping = true
	return nil
}

var (
	// ErrSuperseded is the cancel cause when a newer job takes over.
	ErrSuperseded = errors.New("superseded by a newer job")
	errCancelled  = errors.New("cancelled")
)

type active struct {
	lock       []string
	cancel     context.CancelCauseFunc
	swapping   bool
	superseded bool
}

// Options are the runner's clock and hooks; zero values are the defaults.
type Options struct {
	Poll time.Duration // how often waiting and queued jobs are re-read; 3s
	Cap  time.Duration // one job's wall-clock limit; 30 minutes
	// Workers is re-read every pass, so a settings change applies within
	// one poll. nil = 2.
	Workers func(ctx context.Context) (int, error)
	// ParamSet reports whether a parked job's param resolves now. nil =
	// requeue every parked job each poll and let the handler re-check.
	ParamSet func(ctx context.Context, param string) (bool, error)
}

type Runner struct {
	jobs     *job.Leaf
	handlers map[Kind]Handler
	logDir   string
	opt      Options

	mu      sync.Mutex
	running map[string]*active
	wake    chan struct{}
	stop    context.CancelFunc
	wg      sync.WaitGroup
}

func New(jobs *job.Leaf, handlers map[Kind]Handler, dataDir string, opt Options) *Runner {
	if opt.Poll == 0 {
		opt.Poll = 3 * time.Second
	}
	if opt.Cap == 0 {
		opt.Cap = 30 * time.Minute
	}
	if opt.Workers == nil {
		opt.Workers = func(context.Context) (int, error) { return 2, nil }
	}
	return &Runner{
		jobs: jobs, handlers: handlers, logDir: filepath.Join(dataDir, "jobs"), opt: opt,
		running: map[string]*active{}, wake: make(chan struct{}, 1),
	}
}

// Start recovers from a restart and begins the loop. Nothing can be running
// at boot: the process that owned those rows is gone, so each is failed
// with what to do next (a crashed job is never resumed blind).
func (r *Runner) Start(ctx context.Context) error {
	if err := os.MkdirAll(r.logDir, 0o750); err != nil {
		return err
	}
	stale, err := r.jobs.List(ctx, job.Running)
	if err != nil {
		return err
	}
	for _, j := range stale {
		if err := r.jobs.Finish(ctx, j, job.Failed, "stackrd restarted while this job ran; run it again"); err != nil {
			return err
		}
	}
	loop, stop := context.WithCancel(context.Background())
	r.stop = stop
	r.wg.Add(1)
	go r.loop(loop)
	return nil
}

// Close stops picking jobs, cancels the running ones and waits for them.
func (r *Runner) Close() {
	if r.stop == nil {
		return
	}
	r.stop()
	r.mu.Lock()
	for _, a := range r.running {
		a.cancel(errors.New("stackrd is shutting down"))
	}
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *Runner) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) loop(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(r.opt.Poll)
	defer t.Stop()
	for {
		if err := r.pass(ctx); err != nil && ctx.Err() == nil {
			slog.Error("job queue pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.unpark(ctx)
		case <-r.wake:
		}
	}
}

// unpark requeues waiting jobs whose param now resolves.
func (r *Runner) unpark(ctx context.Context) {
	parked, err := r.jobs.List(ctx, job.Waiting)
	if err != nil {
		return
	}
	for _, j := range parked {
		if r.opt.ParamSet != nil && j.WaitingParam != nil {
			if ok, err := r.opt.ParamSet(ctx, *j.WaitingParam); err != nil || !ok {
				continue
			}
		}
		_ = r.jobs.Requeue(ctx, j)
	}
}

// pass starts every job that may start now: the oldest queued job whose
// lock set is free, repeatedly, until the workers are full. A queued job
// that cannot start still holds its tiles against newer ones, so each tile
// sees its jobs in order.
func (r *Runner) pass(ctx context.Context) error {
	n, err := r.opt.Workers(ctx)
	if err != nil {
		return err
	}
	queued, err := r.jobs.List(ctx, job.Queued)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var busy []string
	for _, a := range r.running {
		busy = append(busy, a.lock...)
	}
	for _, j := range queued {
		if len(r.running) >= n {
			return nil
		}
		if job.Overlaps(j.LockSet, busy) {
			busy = append(busy, j.LockSet...)
			continue
		}
		busy = append(busy, j.LockSet...)
		if err := r.start(j); err != nil {
			return err
		}
	}
	return nil
}

// start runs j on its own goroutine. Called with mu held.
func (r *Runner) start(j store.Job) error {
	j, err := r.jobs.Start(context.Background(), j)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithCancelCause(context.Background())
	a := &active{lock: j.LockSet, cancel: cancel}
	r.running[j.ID] = a
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer cancel(nil)
		r.run(cctx, j, a)
		r.mu.Lock()
		delete(r.running, j.ID)
		r.mu.Unlock()
		r.poke()
	}()
	return nil
}

func (r *Runner) run(parent context.Context, j store.Job, a *active) {
	ctx, cancel := context.WithTimeout(parent, r.opt.Cap)
	defer cancel()
	state, reason, param := r.call(ctx, j, a)

	// The run's context is dead exactly when there is something to record.
	fresh := context.Background()
	var err error
	if state == job.Waiting {
		err = r.jobs.Park(fresh, j, param)
	} else {
		err = r.jobs.Finish(fresh, j, state, reason)
	}
	if err != nil {
		slog.Error("recording job result failed", "job", j.ID, "state", state, "error", err)
	}
}

// call runs the handler and turns its outcome into a state.
func (r *Runner) call(ctx context.Context, j store.Job, a *active) (state, reason, param string) {
	h, ok := r.handlers[Kind(j.Kind)]
	if !ok {
		return job.Failed, fmt.Sprintf("no handler for job kind %q", j.Kind), ""
	}
	f, err := os.OpenFile(j.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return job.Failed, "opening the job log: " + err.Error(), ""
	}
	defer func() { _ = f.Close() }()

	defer func() {
		if p := recover(); p != nil {
			slog.Error("job handler panicked", "job", j.ID, "panic", p, "stack", string(debug.Stack()))
			state, reason = job.Failed, fmt.Sprintf("panic: %v", p)
		}
	}()
	err = h(ctx, &Run{Job: j, Log: f, a: a, r: r})

	switch cause := context.Cause(ctx); {
	case err == nil:
		return job.Done, "", ""
	case errors.Is(cause, ErrSuperseded) || errors.Is(err, ErrSuperseded):
		return job.Superseded, "", ""
	case errors.Is(cause, errCancelled):
		return job.Cancelled, "", ""
	case errors.Is(cause, context.DeadlineExceeded):
		return job.Failed, "stopped after the " + r.opt.Cap.String() + " cap", ""
	}
	if u, ok := errs.IsUnset(err); ok {
		return job.Waiting, "", u.Param
	}
	return job.Failed, err.Error(), ""
}

// Enqueue writes a queued job and applies the Railway rule to older live
// jobs it supersedes: queued or waiting ones become superseded, a running
// one still building is cancelled, a running one already swapping is left
// to finish (the lock makes the new job wait).
func (r *Runner) Enqueue(ctx context.Context, kind Kind, lock []string, payload string, releaseID *string) (store.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	older, err := r.jobs.List(ctx, job.Queued, job.Waiting, job.Running)
	if err != nil {
		return store.Job{}, err
	}
	j, err := r.jobs.Create(ctx, string(kind), lock, payload, releaseID, r.logDir)
	if err != nil {
		return store.Job{}, err
	}
	for _, o := range older {
		if !job.Overlaps(j.LockSet, o.LockSet) || !job.Supersedes(j, o) {
			continue
		}
		if a, ok := r.running[o.ID]; ok {
			if !a.swapping {
				a.superseded = true
				a.cancel(ErrSuperseded)
			}
			continue
		}
		if o.State != job.Running {
			if err := r.jobs.Finish(ctx, o, job.Superseded, ""); err != nil {
				return j, err
			}
		}
	}
	r.poke()
	return j, nil
}

// Cancel stops a job. A running job is cancelled through its context, except
// mid-swap, which is never aborted. A finished job is not an error: the
// caller is racing the work, the normal case for a stop button.
func (r *Runner) Cancel(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.running[id]; ok {
		if a.swapping {
			return errs.Conflictf("the job is swapping containers and cannot be stopped now")
		}
		a.cancel(errCancelled)
		return nil
	}
	j, err := r.jobs.Get(ctx, id)
	if err != nil {
		return err
	}
	if !job.Cancellable(j.State) {
		return nil
	}
	return r.jobs.Finish(ctx, j, job.Cancelled, "")
}

// Get returns one job row.
func (r *Runner) Get(ctx context.Context, id string) (store.Job, error) { return r.jobs.Get(ctx, id) }
