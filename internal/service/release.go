package service

import (
	"context"
	"io"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	Release = store.Release
	Pin     = release.Pin
	Plan    = promote.Plan
	Change  = promote.Change
)

// PromotePlan is the dry run and its verdict: CanDeploy is false exactly
// when Plan.Blockers is not empty, the same list Promote refuses with (B20),
// except a plan blocked only on host access: that queues and parks on an
// admin's approval.
type PromotePlan struct {
	Plan      *Plan `json:"plan"`
	CanDeploy bool  `json:"can_deploy"`
}

func (o *Orchestrator) Releases(ctx context.Context, stackID string) ([]Release, error) {
	return o.releases.List(ctx, stackID)
}

// Release is one release and its pins by slug.
func (o *Orchestrator) Release(ctx context.Context, id string) (Release, map[string]Pin, error) {
	r, err := o.releases.Get(ctx, id)
	if err != nil {
		return r, nil, err
	}
	pins, err := o.releases.Pins(ctx, id)
	return r, pins, err
}

// PlanPromote is the dry run of taking releaseID into envID, as by would
// approve it: a plan that changes pin, ingress or exits is blocked unless by
// is an org owner.
func (o *Orchestrator) PlanPromote(ctx context.Context, by *Principal, envID, releaseID string) (PromotePlan, error) {
	ok, err := o.mayPlace(ctx, by, envID)
	if err != nil {
		return PromotePlan{}, err
	}
	p, err := o.promote.Plan(withMayPlace(ctx, ok), envID, releaseID, io.Discard)
	if err != nil {
		return PromotePlan{}, err
	}
	return PromotePlan{Plan: p, CanDeploy: !p.Blocked() || p.OnlyHostAccess()}, nil
}

// Promote queues taking releaseID into envID, approved by by. The job
// re-plans and refuses with the same blockers the dry run shows.
func (o *Orchestrator) Promote(ctx context.Context, by *Principal, envID, releaseID string) (Job, error) {
	return o.PromoteTicked(ctx, by, envID, releaseID, nil)
}

// PromoteTicked is Promote that also applies the plan's secret removal rows
// (Change.Key) named in ticked; unticked ones stay.
func (o *Orchestrator) PromoteTicked(ctx context.Context, by *Principal, envID, releaseID string, ticked []string) (Job, error) {
	ok, err := o.mayPlace(ctx, by, envID)
	if err != nil {
		return Job{}, err
	}
	return o.enqueuePromote(ctx, envID, releaseID, ok, ticked...)
}

// Rollback is Promote with an older release: one path, one rule (B2).
func (o *Orchestrator) Rollback(ctx context.Context, by *Principal, envID, releaseID string) (Job, error) {
	return o.Promote(ctx, by, envID, releaseID)
}
