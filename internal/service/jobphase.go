package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// jobPhase is the step a tile card shows for its live job: the step its log
// last named while it runs. A queued job shows just the word "Queued"; a
// place in the queue is global (it leaks other orgs' load) and not the real
// order.
func (o *Orchestrator) jobPhase(_ context.Context, j store.Job, slug string) string {
	if j.State == job.Running {
		return o.jobs.Phase(j.ID, slug)
	}
	return ""
}
