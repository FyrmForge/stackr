package service

import "context"

// PushFailure is a stack's last push that failed: the job to open and what
// it said.
type PushFailure struct{ JobID, Error string }

// PushFailure is the stack's newest failed push that no later done push of
// the same repo and branch follows. Queued, running, cancelled and
// superseded pushes say nothing: only a push that lands clears it.
func (o *Orchestrator) PushFailure(ctx context.Context, stackID string) (PushFailure, bool) {
	j, ok, err := o.store.Jobs.NewestFailedUnhealed(ctx, string(kindPush), "push:"+stackID+":%")
	if err != nil || !ok {
		return PushFailure{}, false
	}
	return PushFailure{JobID: j.ID, Error: j.Error}, true
}
