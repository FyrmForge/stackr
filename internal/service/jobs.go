package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Job = store.Job

// GetJob returns one job row.
func (o *Orchestrator) GetJob(ctx context.Context, id string) (Job, error) {
	return o.jobs.Get(ctx, id)
}

// CancelJob stops a queued, waiting or building job. Mid-swap is a
// Conflict; an already finished job is not an error.
func (o *Orchestrator) CancelJob(ctx context.Context, id string) error {
	return o.jobs.Cancel(ctx, id)
}
