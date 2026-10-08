package service

import "context"

// QueueServerPlan is queueServerPlan for the external tests. It answers the
// queued job, or the zero Job when the push is not the server file's.
func (o *Orchestrator) QueueServerPlan(ctx context.Context, connectorID, repo, branch, defaultBranch string) (Job, error) {
	return o.queueServerPlan(ctx, connectorID, repo, branch, defaultBranch)
}
