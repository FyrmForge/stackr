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
// when Plan.Blockers is not empty, the same list Promote refuses with (B20).
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

// PlanPromote is the dry run of taking releaseID into envID.
func (o *Orchestrator) PlanPromote(ctx context.Context, envID, releaseID string) (PromotePlan, error) {
	p, err := o.promote.Plan(ctx, envID, releaseID, io.Discard)
	if err != nil {
		return PromotePlan{}, err
	}
	return PromotePlan{Plan: p, CanDeploy: !p.Blocked()}, nil
}

// Promote queues taking releaseID into envID. The job re-plans and refuses
// with the same blockers the dry run shows.
func (o *Orchestrator) Promote(ctx context.Context, envID, releaseID string) (Job, error) {
	return o.enqueuePromote(ctx, envID, releaseID)
}

// Rollback is Promote with an older release: one path, one rule (B2).
func (o *Orchestrator) Rollback(ctx context.Context, envID, releaseID string) (Job, error) {
	return o.Promote(ctx, envID, releaseID)
}
