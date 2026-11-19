package service

import "context"

// WaitingRelease is the release the env below runs and this env does not:
// the one that waits to be promoted here. ok is false when none does.
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
