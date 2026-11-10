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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/selfmetrics"
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
	if r.r == nil { // a Run made outside a Runner (a test) has nothing to swap
		return nil
	}
	r.r.mu.Lock()
	defer r.r.mu.Unlock()
	if r.a.superseded {
		return ErrSuperseded
	}
	r.a.swapping = true
	return nil
}

type idleKey struct{}

// Idle is called by a handler that is about to only wait on other jobs: until
// the returned func runs, this job does not count against the workers, so
// the jobs it waits for can start (two such waits would otherwise hold both
// workers). Its lock set still holds. Outside a job it does nothing.
func Idle(ctx context.Context) (resume func()) {
	rn, ok := ctx.Value(idleKey{}).(*Run)
	if !ok {
		return func() {}
	}
	rn.r.mu.Lock()
	rn.a.idle = true
	rn.r.mu.Unlock()
	rn.r.poke()
	return func() {
		rn.r.mu.Lock()
		rn.a.idle = false
		rn.r.mu.Unlock()
	}
}

// ErrFinished is Cancel on a job already past cancelling (a Conflict).
var ErrFinished = errs.Conflictf("the job already finished")

var (
	// ErrSuperseded is the cancel cause when a newer job takes over.
	ErrSuperseded = errors.New("superseded by a newer job")
	errCancelled  = errors.New("cancelled")
	// ErrSkipped from a handler records the job as skipped, not failed, with
	// the error as the reason (a scheduled run whose node dropped before it
	// was marked offline).
	ErrSkipped = errors.New("skipped")
)

type active struct {
	lock       []string
	cancel     context.CancelCauseFunc
	swapping   bool
	superseded bool
	idle       bool // waiting on other jobs: not counted against the workers
}

// Options are the runner's clock and hooks; zero values are the defaults.
type Options struct {
	Poll time.Duration // how often waiting and queued jobs are re-read; 3s
	Cap  time.Duration // one job's wall-clock limit; 30 minutes
	// Uncapped kinds have no wall-clock limit: their handler holds its own
	// (a cron or function run's timeout_minutes).
	Uncapped map[Kind]bool
	// Caps overrides Cap for a kind (data-moving jobs run for hours).
	Caps map[Kind]time.Duration
	// Workers is re-read every pass, so a settings change applies within
	// one poll. nil = 2.
	Workers func(ctx context.Context) (int, error)
	// ParamSet reports whether a parked job's param (j.WaitingParam) resolves
	// now. nil = requeue every parked job each poll and let the handler
	// re-check.
	ParamSet func(ctx context.Context, j store.Job) (bool, error)
	// Hold, when it answers true, starts nothing: an upgrade helper is
	// swapping the panel, and a job started now dies with the old panel or
	// outlives a rollback of the new one's database. nil = never.
	Hold func() bool
	// Survived reports a job still running at boot that the restart itself
	// completed (the upgrade that swapped this panel in): it is finished
	// done, not failed. nil = none.
	Survived func(j store.Job) bool
	// Ended runs after a job's row is finished for good (any end but a
	// park), whether or not its handler ran: a job cancelled or superseded
	// while queued or waiting still lets go of what it holds. Called with
	// the runner's lock held; it must not call back into the Runner. nil = nothing.
	Ended func(j store.Job)
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

	// parked is the param each waiting job's log last named, so a job
	// re-run every poll says what it waits on once, not every poll.
	// ponytail: a waiting job superseded or cancelled keeps its entry until
	// a restart; a few bytes each.
	parked map[string]string

	// deployed holds the "deploying <tile>" lines each job's log already
	// has, so a parked job re-run every poll logs each tile's line once.
	deployed map[string]map[string]bool

	// beforePark runs between a handler's return and the park write; tests
	// land a cancel there.
	beforePark func()
}

// onceLog drops a "deploying <tile>" line its job's log already has, and a
// "warning: " line (the disk check, once per tile of a promote).
// Promote and env sync both log the first before a deploy that can park.
type onceLog struct {
	r  *Runner
	id string
	w  io.Writer
}

func (o onceLog) Write(p []byte) (int, error) {
	line := string(p)
	if (strings.HasPrefix(line, "deploying ") || strings.HasPrefix(line, "warning: ")) &&
		strings.Count(line, "\n") == 1 && strings.HasSuffix(line, "\n") {
		o.r.mu.Lock()
		seen := o.r.deployed[o.id][line]
		if o.r.deployed[o.id] == nil {
			o.r.deployed[o.id] = map[string]bool{}
		}
		o.r.deployed[o.id][line] = true
		o.r.mu.Unlock()
		if seen {
			return len(p), nil
		}
	}
	return o.w.Write(p)
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
		jobs:     jobs,
		handlers: handlers,
		logDir:   filepath.Join(dataDir, "jobs"),
		opt:      opt,
		running:  map[string]*active{},
		parked:   map[string]string{},
		deployed: map[string]map[string]bool{},
		wake:     make(chan struct{}, 1),
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
		state, reason := job.Failed, "stackrd restarted while this job ran; run it again"
		if r.opt.Survived != nil && r.opt.Survived(j) {
			state, reason = job.Done, ""
		}
		if err := r.jobs.Finish(ctx, j, state, reason); err != nil {
			return err
		}
		r.ended(j)
	}
	loop, stop := context.WithCancel(context.Background())
	r.stop = stop
	r.wg.Add(1)
	go r.loop(loop)
	return nil
}

// Wake runs a pass now: a hold ended.
func (r *Runner) Wake() { r.poke() }

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
	// A handler stuck past the grace must not keep the process (and db.Close)
	// from ending; boot fails what is left running.
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(closeGrace):
		slog.Warn("jobs still running at shutdown; giving up waiting")
	}
}

var closeGrace = 30 * time.Second

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
			if ok, err := r.opt.ParamSet(ctx, j); err != nil || !ok {
				continue
			}
		}
		// Under the lock Cancel and Enqueue hold, so a finish cannot land
		// between Requeue's read and write.
		r.mu.Lock()
		_ = r.jobs.Requeue(ctx, j)
		r.mu.Unlock()
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
	// Read under the lock Cancel and Enqueue finish queued rows under: a
	// list read before it could start a job cancelled since, and Start's
	// write would bring the row back to running.
	r.mu.Lock()
	defer r.mu.Unlock()
	queued, err := r.jobs.List(ctx, job.Queued)
	if err != nil {
		return err
	}
	// One entry per job, not one flat list: job.All overlaps a job with an
	// empty lock set too.
	var busy [][]string
	for _, a := range r.running {
		busy = append(busy, a.lock)
	}
	for _, j := range queued {
		if r.busyWorkers() >= n {
			return nil
		}
		blocked := slices.ContainsFunc(busy, func(b []string) bool { return job.Overlaps(j.LockSet, b) })
		busy = append(busy, j.LockSet)
		if blocked {
			continue
		}
		// Asked only when a job would start, so an idle queue costs Hold nothing.
		if r.opt.Hold != nil && r.opt.Hold() {
			return nil
		}
		if err := r.start(j); err != nil {
			return err
		}
	}
	return nil
}

// busyWorkers is the running jobs that hold a worker. Called with mu held.
func (r *Runner) busyWorkers() int {
	n := 0
	for _, a := range r.running {
		if !a.idle {
			n++
		}
	}
	return n
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
		r.poke()
	}()
	return nil
}

func (r *Runner) capFor(k Kind) time.Duration {
	if c, ok := r.opt.Caps[k]; ok {
		return c
	}
	return r.opt.Cap
}

func (r *Runner) run(parent context.Context, j store.Job, a *active) {
	ctx, cancel := parent, context.CancelFunc(func() {})
	if !r.opt.Uncapped[Kind(j.Kind)] {
		ctx, cancel = context.WithTimeout(parent, r.capFor(Kind(j.Kind)))
	}
	defer cancel()
	rn := &Run{Job: j, a: a, r: r}
	start := time.Now()
	state, reason, param := r.call(ctx, rn)

	// The run's context is dead exactly when there is something to record.
	fresh := context.Background()
	if state == job.Waiting && r.beforePark != nil {
		r.beforePark()
	}
	// The row's write and the job leaving running are one step under the
	// lock Cancel and Enqueue hold: a Cancel that reads the row finished or
	// parked never finds the job still running and claims it stopped it.
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, j.ID)
	var err error
	if state == job.Waiting {
		// A job stopped since its handler returned is finished, never parked.
		switch cause := context.Cause(parent); {
		case errors.Is(cause, ErrSuperseded):
			state = job.Superseded
		case cause != nil:
			state = job.Cancelled
		default:
			// rn.Job, not j: a handler may have rewritten its payload.
			err = r.jobs.Park(fresh, rn.Job, param)
		}
	}
	if state != job.Waiting {
		delete(r.parked, j.ID)
		delete(r.deployed, j.ID)
		err = r.jobs.Finish(fresh, j, state, reason)
		selfmetrics.Job(j.Kind, state, time.Since(start))
		if err == nil {
			r.ended(rn.Job)
		}
	}
	if err != nil {
		slog.Error("recording job result failed", "job", j.ID, "state", state, "error", err)
	}
}

// call runs the handler and turns its outcome into a state.
func (r *Runner) call(ctx context.Context, rn *Run) (state, reason, param string) {
	j := rn.Job
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
	rn.Log = onceLog{r: r, id: j.ID, w: f}
	err = h(context.WithValue(ctx, idleKey{}, rn), rn)

	switch cause := context.Cause(ctx); {
	case err == nil:
		return job.Done, "", ""
	case errors.Is(cause, ErrSuperseded) || errors.Is(err, ErrSuperseded):
		return job.Superseded, "", ""
	case errors.Is(cause, errCancelled):
		return job.Cancelled, "", ""
	case errors.Is(cause, context.DeadlineExceeded):
		return job.Failed, "stopped after the " + r.capFor(Kind(j.Kind)).String() + " cap", ""
	case errors.Is(err, ErrSkipped):
		return job.Skipped, err.Error(), ""
	}
	if u, ok := errs.IsUnset(err); ok {
		r.mu.Lock()
		said := r.parked[j.ID] == u.Param
		r.parked[j.ID] = u.Param
		r.mu.Unlock()
		if !said {
			_, _ = fmt.Fprintf(f, "waiting: %v; the job resumes once it is\n", u)
		}
		return job.Waiting, "", u.Param
	}
	if n, ok := errs.IsNeedsApproval(err); ok {
		r.mu.Lock()
		said := r.parked[j.ID] == n.What
		r.parked[j.ID] = n.What
		r.mu.Unlock()
		if !said {
			_, _ = fmt.Fprintf(f, "%v\n", n)
		}
		return job.Waiting, "", n.What
	}
	return job.Failed, err.Error(), ""
}

// Enqueue writes a queued job and applies the Railway rule to older live
// jobs it supersedes: queued or waiting ones become superseded, a running
// one still building is cancelled, a running one already swapping is left
// to finish (the lock makes the new job wait).
func (r *Runner) Enqueue(
	ctx context.Context,
	kind Kind,
	lock []string,
	payload string,
	releaseID *string,
) (store.Job, error) {
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
			r.ended(o)
		}
	}
	r.poke()
	return j, nil
}

// Skip records a job that never ran, finished at once as skipped with the
// reason (a schedule that fired while its node was offline). It takes no
// tile lock and supersedes nothing.
func (r *Runner) Skip(ctx context.Context, kind Kind, lock []string, payload, reason string) (store.Job, error) {
	// Under the lock pass lists under, so the row is never seen queued.
	r.mu.Lock()
	defer r.mu.Unlock()
	j, err := r.jobs.Create(ctx, string(kind), lock, payload, nil, r.logDir)
	if err != nil {
		return j, err
	}
	if err := r.jobs.Finish(ctx, j, job.Skipped, reason); err != nil {
		return j, err
	}
	return r.jobs.Get(ctx, j.ID)
}

// Cancel stops a job. A running job is cancelled through its context, except
// mid-swap, which is never aborted. A finished job is a Conflict: the stop
// button raced the work and must not claim it cancelled anything.
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
		return ErrFinished
	}
	if err := r.jobs.Finish(ctx, j, job.Cancelled, ""); err != nil {
		return err
	}
	r.ended(j)
	return nil
}

func (r *Runner) ended(j store.Job) {
	if r.opt.Ended != nil {
		r.opt.Ended(j)
	}
}

// Get returns one job row.
func (r *Runner) Get(ctx context.Context, id string) (store.Job, error) { return r.jobs.Get(ctx, id) }
