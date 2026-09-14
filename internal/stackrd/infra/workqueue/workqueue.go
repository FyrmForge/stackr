// Package workqueue is one durable runner for every long job
// (docs/plans/33-workqueue.md).
//
// Long work used to be started from an HTTP handler five different ways, each
// with its own machinery. The config apply had none at all: it ran on the
// request context, so a browser that stopped waiting killed it wherever it had
// got to, and the error it tried to write used the same dead context and was
// never recorded. The webhook path was
// worst, GitHub hangs up after about ten seconds.
//
// Two rules make the difference. A job runs on context.Background() with its
// kind's timeout, never on a request context. And the queue is a table, so a
// row left running by a restart is dealt with on boot instead of vanishing
// with the process.
package workqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Job is one item as a handler sees it, plus the two writes it may make while
// running.
type Job struct {
	Item  *repo.WorkItem
	store repo.Store
}

// Payload decodes the item's payload into v.
func (j *Job) Payload(v any) error {
	if j.Item.Payload == "" {
		return nil
	}
	return json.Unmarshal([]byte(j.Item.Payload), v)
}

// SetStep records coarse progress. It is also the resume point for a kind
// whose recovery is "carry on from here", which is why it is a column and not
// a log line.
func (j *Job) SetStep(ctx context.Context, step string) {
	j.Item.Step = step
	if err := j.store.SetWorkItemProgress(ctx, j.Item.ID, step, j.Item.Progress); err != nil {
		slog.Error("workqueue: writing step", "item", j.Item.ID, "error", err)
	}
}

// SetProgress records the fine-grained blob (bytes done and total, and
// friends). Whatever the UI for this kind reads.
func (j *Job) SetProgress(ctx context.Context, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	j.Item.Progress = string(b)
	if err := j.store.SetWorkItemProgress(ctx, j.Item.ID, j.Item.Step, j.Item.Progress); err != nil {
		slog.Error("workqueue: writing progress", "item", j.Item.ID, "error", err)
	}
}

// Handler runs one job. The context is the queue's own, with the kind's
// timeout on it and a cancel handle held for Cancel.
type Handler func(ctx context.Context, j *Job) error

// Recovery says what happens to a row left running by a restart.
type Recovery int

const (
	// Requeue runs it again from the top. Only for work that is convergent:
	// re-running it on a half-finished state finishes the job.
	Requeue Recovery = iota
	// Fail gives up and records why. For work that cannot be resumed blind,
	// like a two-pass rsync with the service scaled to zero, or a half-written
	// backup artifact.
	Fail
)

// KindOpts is the per-kind configuration.
type KindOpts struct {
	// Timeout bounds one run. Zero means 30 minutes, which is what the deploy
	// engine has always used.
	Timeout time.Duration
	// Concurrency is how many of this kind run at once. Zero means one.
	Concurrency int
	// OnRestart is what boot recovery does with a row left running.
	OnRestart Recovery
	// RestartFail is the error recorded when OnRestart is Fail. A generic
	// "the panel restarted" tells an operator nothing about what to do next.
	RestartFail string
	// Cleanup runs on a job that OnRestart failed, before the row is closed:
	// deleting a partial artifact, putting a service back up. Best effort.
	Cleanup func(ctx context.Context, j *Job)
	// OnSuperseded runs for each queued item a newer Enqueue with the same
	// dedupe key pushed aside, so the domain row it carried can be closed.
	// Without it the deploy or plan row stays open with nothing to run it.
	OnSuperseded func(ctx context.Context, j *Job)
}

type kind struct {
	h    Handler
	opts KindOpts
	sem  chan struct{}
}

// Queue is the runner. One per process.
type Queue struct {
	store repo.Store

	mu      sync.Mutex
	kinds   map[string]*kind
	cancels map[string]context.CancelFunc

	// wake is poked whenever something is enqueued, so the loop does not wait
	// out its poll interval for work that is already there.
	wake chan struct{}
}

// New builds the queue. Nothing runs until Start.
func New(store repo.Store) *Queue {
	return &Queue{
		store:   store,
		kinds:   map[string]*kind{},
		cancels: map[string]context.CancelFunc{},
		wake:    make(chan struct{}, 1),
	}
}

// Register wires a handler for one kind. Call before Start.
func (q *Queue) Register(name string, h Handler, opts KindOpts) {
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Minute
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 1
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.kinds[name] = &kind{h: h, opts: opts, sem: make(chan struct{}, opts.Concurrency)}
}

// Enqueue adds an item and supersedes the older queued ones sharing its
// dedupe key. Returns the new item's id.
//
// The key is a scope, not an identity: for a config apply it is the stack, so
// two pushes a second apart do not both apply; for a deploy it is the tile,
// which is what the engine already did by hand. An empty key never dedupes.
func (q *Queue) Enqueue(ctx context.Context, kindName, dedupeKey string, payload any) (string, error) {
	q.mu.Lock()
	k, known := q.kinds[kindName]
	q.mu.Unlock()
	if !known {
		return "", fmt.Errorf("workqueue: no handler registered for %q", kindName)
	}
	body := ""
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return "", err
		}
		body = string(b)
	}
	w := &repo.WorkItem{
		ID:        uuid.New().String(),
		Kind:      kindName,
		DedupeKey: dedupeKey,
		Payload:   body,
		Status:    "queued",
		CreatedAt: time.Now().UTC(),
	}
	if err := q.store.CreateWorkItem(ctx, w); err != nil {
		return "", err
	}
	// Read the victims before the update flips them: the store reports no
	// ids back. Only worth the query when someone wants to hear about them.
	//
	// The list is rowid-ordered, so stopping at our own row is the same set
	// the UPDATE touches. Skipping it instead would name a concurrent
	// enqueue's newer row as superseded when it is the one that survives.
	var older []repo.WorkItem
	if k.opts.OnSuperseded != nil && dedupeKey != "" {
		queued, _ := q.store.ListWorkItemsByStatus(ctx, "queued")
		for _, o := range queued {
			if o.ID == w.ID {
				break
			}
			if o.Kind == kindName && o.DedupeKey == dedupeKey {
				older = append(older, o)
			}
		}
	}
	// After the insert, so the row that survives is the new one whatever
	// happens in between.
	if err := q.store.SupersedeQueuedWorkItems(ctx, kindName, dedupeKey, w.ID); err != nil {
		slog.Error("workqueue: superseding older items", "kind", kindName, "key", dedupeKey, "error", err)
	} else {
		for i := range older {
			k.opts.OnSuperseded(ctx, &Job{Item: &older[i], store: q.store})
		}
	}
	q.poke()
	return w.ID, nil
}

// Cancel stops a running job, or drops a queued one. A job that has already
// finished is not an error: the caller is racing the work, which is the normal
// case for a Stop button.
func (q *Queue) Cancel(ctx context.Context, id string) error {
	q.mu.Lock()
	cancel := q.cancels[id]
	q.mu.Unlock()
	if cancel != nil {
		cancel()
		return nil
	}
	w, err := q.store.GetWorkItem(ctx, id)
	if err != nil || w == nil || w.Done() {
		return err
	}
	return q.store.FinishWorkItem(ctx, id, "cancelled", "")
}

func (q *Queue) poke() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Start recovers what the last process left behind and then runs the loop
// until ctx is done. Non-blocking.
func (q *Queue) Start(ctx context.Context) {
	q.recover(ctx)
	go q.loop(ctx)
}

// recover deals with every row left running by a process that is no longer
// here. This is the point of keeping the queue in a table.
func (q *Queue) recover(ctx context.Context) {
	items, err := q.store.ListWorkItemsByStatus(ctx, "running")
	if err != nil {
		slog.Error("workqueue: cannot read what the last run left behind", "error", err)
		return
	}
	for i := range items {
		w := &items[i]
		q.mu.Lock()
		k := q.kinds[w.Kind]
		q.mu.Unlock()
		// A kind nobody registers any more cannot be recovered or reasoned
		// about, and leaving it running would hide it from every listing.
		if k == nil {
			if err := q.store.FinishWorkItem(ctx, w.ID, "error", "no handler for "+w.Kind+" any more"); err != nil {
				slog.Error("workqueue: orphaned item not closed", "kind", w.Kind, "item", w.ID, "error", err)
			}
			continue
		}
		if k.opts.OnRestart == Requeue {
			slog.Warn("workqueue: requeueing work the restart interrupted", "kind", w.Kind, "item", w.ID)
			if err := q.store.RequeueWorkItem(ctx, w.ID); err != nil {
				slog.Error("workqueue: requeue failed", "item", w.ID, "error", err)
			}
			continue
		}
		msg := k.opts.RestartFail
		if msg == "" {
			msg = "the panel restarted while this was running, and it cannot be resumed"
		}
		if k.opts.Cleanup != nil {
			k.opts.Cleanup(ctx, &Job{Item: w, store: q.store})
		}
		slog.Warn("workqueue: failing work the restart interrupted", "kind", w.Kind, "item", w.ID)
		if err := q.store.FinishWorkItem(ctx, w.ID, "error", msg); err != nil {
			slog.Error("workqueue: interrupted item not closed", "kind", w.Kind, "item", w.ID, "error", err)
		}
	}
}

// loop picks up queued work. Polling as well as waking on Enqueue, so an item
// written by anything that does not go through this process (a recovery, a
// future second writer) is still picked up.
func (q *Queue) loop(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		q.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-tick.C:
		}
	}
}

func (q *Queue) drain(ctx context.Context) {
	items, err := q.store.ListWorkItemsByStatus(ctx, "queued")
	if err != nil {
		slog.Error("workqueue: cannot read the queue", "error", err)
		return
	}
	for i := range items {
		w := items[i]
		q.mu.Lock()
		k := q.kinds[w.Kind]
		q.mu.Unlock()
		if k == nil {
			if err := q.store.FinishWorkItem(ctx, w.ID, "error", "no handler for "+w.Kind); err != nil {
				slog.Error("workqueue: item with no handler not closed", "kind", w.Kind, "item", w.ID, "error", err)
			}
			continue
		}
		// Non-blocking: a kind at its concurrency limit must not hold up the
		// other kinds behind it in the list.
		select {
		case k.sem <- struct{}{}:
		default:
			continue
		}
		claimed, err := q.store.ClaimWorkItem(ctx, w.ID)
		if err != nil || !claimed {
			<-k.sem
			continue
		}
		go func(w repo.WorkItem, k *kind) {
			defer func() { <-k.sem }()
			q.run(w, k)
		}(w, k)
	}
}

// run executes one claimed job. Background, not the request that asked for it:
// a client going away must never stop work that is already changing containers
// and databases.
func (q *Queue) run(w repo.WorkItem, k *kind) {
	ctx, cancel := context.WithTimeout(context.Background(), k.opts.Timeout)
	defer cancel()
	q.mu.Lock()
	q.cancels[w.ID] = cancel
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		delete(q.cancels, w.ID)
		q.mu.Unlock()
	}()

	j := &Job{Item: &w, store: q.store}
	err := func() (err error) {
		// A panic in one handler must not take the panel with it. The row
		// records it, which is the only place anyone would look.
		//
		// The stack goes to the log, not the row: without it the recorded
		// error is "panic: nil pointer dereference" and nothing else, and
		// finding where it came from meant reproducing it with a debugger
		// attached.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("workqueue: handler panicked", "kind", w.Kind,
					"item", w.ID, "panic", r, "stack", string(debug.Stack()))
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		return k.h(ctx, j)
	}()

	// The finishing write uses a fresh context on purpose. The one above may
	// be cancelled or timed out, which is exactly when there is something
	// worth recording, and writing the outcome through a dead context is the
	// bug this whole package exists to kill.
	fin := context.Background()
	switch {
	case err == nil:
		if ferr := q.store.FinishWorkItem(fin, w.ID, "done", ""); ferr != nil {
			slog.Error("workqueue: item not closed", "kind", w.Kind, "item", w.ID, "status", "done", "error", ferr)
		}
	case ctx.Err() == context.Canceled:
		if ferr := q.store.FinishWorkItem(fin, w.ID, "cancelled", ""); ferr != nil {
			slog.Error("workqueue: item not closed", "kind", w.Kind, "item", w.ID, "status", "cancelled", "error", ferr)
		}
	default:
		slog.Error("workqueue: job failed", "kind", w.Kind, "item", w.ID, "error", err)
		if ferr := q.store.FinishWorkItem(fin, w.ID, "error", err.Error()); ferr != nil {
			slog.Error("workqueue: item not closed", "kind", w.Kind, "item", w.ID, "status", "error", "error", ferr)
		}
	}
}
