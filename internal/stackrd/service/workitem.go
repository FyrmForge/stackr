package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// WorkItemService owns the work_items table.
//
// Be honest about what this is: a forwarder. The work queue is the only
// caller, work_items is the queue's own bookkeeping, and there is no rule on
// this side of any of these methods — nothing here refuses anything, and
// nothing else in the codebase has a second opinion about what a queued item
// looks like.
//
// It exists because the rule the drift audit settled on is "the store is
// written from service/ and nowhere else", and a rule with a carve-out for
// "unless it looked like bookkeeping to whoever wrote it" is not a rule —
// that judgement is exactly what produced five writers for the config plan
// table. If the queue ever grows a rule (a retry ceiling, a per-kind
// concurrency limit, an item a caller may not cancel), this is where it goes
// and every surface gets it at once.
//
// The counter-argument is real and worth leaving here: a forwarder is a layer
// that costs a hop and hides nothing. If the dev would rather work_items sat
// below the line with the metrics and the deployment rows, this file and the
// interface in infra/workqueue are what to delete, and the guard's allowlist
// is where to say so.
type WorkItemService struct {
	store repo.Store
}

func NewWorkItemService(store repo.Store) *WorkItemService {
	return &WorkItemService{store: store}
}

// Enqueue records a queued item.
func (s *WorkItemService) Enqueue(ctx context.Context, w *repo.WorkItem) error {
	return s.store.CreateWorkItem(ctx, w)
}

// Claim takes an item for this process, reporting whether it won the race.
func (s *WorkItemService) Claim(ctx context.Context, id string) (bool, error) {
	return s.store.ClaimWorkItem(ctx, id)
}

// Finish closes an item: done, cancelled, or error with a message.
func (s *WorkItemService) Finish(ctx context.Context, id, status, errMsg string) error {
	return s.store.FinishWorkItem(ctx, id, status, errMsg)
}

// Requeue puts an item a restart interrupted back in the queue.
func (s *WorkItemService) Requeue(ctx context.Context, id string) error {
	return s.store.RequeueWorkItem(ctx, id)
}

// Progress records the step an item is on, for the progress line the panel
// shows while it runs.
func (s *WorkItemService) Progress(ctx context.Context, id, step, progress string) error {
	return s.store.SetWorkItemProgress(ctx, id, step, progress)
}

// Supersede retires the queued items a newer one outranks, by dedupe key.
func (s *WorkItemService) Supersede(ctx context.Context, kind, dedupeKey, exceptID string) error {
	return s.store.SupersedeQueuedWorkItems(ctx, kind, dedupeKey, exceptID)
}
