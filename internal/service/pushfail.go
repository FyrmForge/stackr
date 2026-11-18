package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
)

// PushFailure is a stack's last push that failed: the job to open and what
// it said.
type PushFailure struct{ JobID, Error string }

// PushFailure is the stack's newest push when it failed, until a push
// after it does not. A push a newer one superseded, a cancelled or skipped
// one says nothing; a queued or running one is a retry under way.
func (o *Orchestrator) PushFailure(ctx context.Context, stackID string) (PushFailure, bool) {
	js, err := o.store.Jobs.ListTouching(ctx, []string{"push:" + stackID}, string(kindPush), 10) // newest first
	if err != nil {
		return PushFailure{}, false
	}
	for _, j := range js {
		switch j.State {
		case job.Superseded, job.Cancelled, job.Skipped:
			continue
		case job.Failed:
			return PushFailure{JobID: j.ID, Error: j.Error}, true
		}
		return PushFailure{}, false
	}
	return PushFailure{}, false
}
