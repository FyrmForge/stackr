package service

import (
	"context"
	"fmt"
	"slices"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// jobPhase is the step a tile card shows for its live job: the place in the
// queue while it waits, the step its log last named while it runs.
func (o *Orchestrator) jobPhase(ctx context.Context, j store.Job) string {
	switch j.State {
	case job.Running:
		return o.jobs.Phase(j.ID)
	case job.Queued:
		q, err := o.jobRows.List(ctx, job.Queued) // oldest first
		if i := slices.IndexFunc(q, func(x store.Job) bool { return x.ID == j.ID }); err == nil && i >= 0 {
			return fmt.Sprintf("Queued (%d of %d)", i+1, len(q))
		}
	}
	return ""
}
