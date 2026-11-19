package service

import "context"

// WaitingRelease is the newest release that waits to be promoted into the
// env: one the env below runs, or one cut for its line. ok is false when none.
func (o *Orchestrator) WaitingRelease(ctx context.Context, stackID, envID string) (Release, bool) {
	rungs, err := o.graph.Rungs(ctx, stackID)
	if err != nil {
		return Release{}, false
	}
	for _, r := range rungs {
		if r.EnvID != envID || !r.Behind {
			continue
		}
		rs, err := o.releases.List(ctx, stackID)
		if err != nil {
			return Release{}, false
		}
		for _, rel := range rs {
			if rel.Number == r.Waiting {
				return rel, true
			}
		}
	}
	return Release{}, false
}
