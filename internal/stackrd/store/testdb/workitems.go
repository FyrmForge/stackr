package testdb

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// WorkItems satisfies infra/workqueue.Items by writing the store directly.
//
// The real implementation is service.WorkItemService. Half the packages that
// build a queue in their tests — infra/jobs, infra/backup — are packages
// service/ is built on, so their in-package tests cannot name it without an
// import cycle. This lives here because testdb is already the shared harness
// and imports nothing but repo.
//
// It is not a stand-in for the service's behaviour: WorkItemService is a
// forwarder, so there is no behaviour to stand in for. If it ever grows a
// rule, this stops being an acceptable double and the tests that use it
// should say so by failing.
type WorkItems struct{ Store repo.Store }

func (w WorkItems) Enqueue(ctx context.Context, item *repo.WorkItem) error {
	return w.Store.CreateWorkItem(ctx, item)
}

func (w WorkItems) Claim(ctx context.Context, id string) (bool, error) {
	return w.Store.ClaimWorkItem(ctx, id)
}

func (w WorkItems) Finish(ctx context.Context, id, status, errMsg string) error {
	return w.Store.FinishWorkItem(ctx, id, status, errMsg)
}

func (w WorkItems) Requeue(ctx context.Context, id string) error {
	return w.Store.RequeueWorkItem(ctx, id)
}

func (w WorkItems) Progress(ctx context.Context, id, step, progress string) error {
	return w.Store.SetWorkItemProgress(ctx, id, step, progress)
}

func (w WorkItems) Supersede(ctx context.Context, kind, dedupeKey, exceptID string) error {
	return w.Store.SupersedeQueuedWorkItems(ctx, kind, dedupeKey, exceptID)
}
