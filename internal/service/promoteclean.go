package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
)

// PromotePair is one release going into one env.
type PromotePair struct{ ReleaseID, EnvID string }

// PromoteOutcome is what became of one pair: a queued Job, a Skip (refused
// by the rules, in words) or an Err (the person may not, or the queue failed).
type PromoteOutcome struct {
	Pair PromotePair
	Job  Job
	Skip string
	Err  error
}

// Skip words.
const (
	SkipReview = "needs full review"
	SkipNewer  = "a newer release is waiting"
)

// PromoteClean promotes each pair from a list, where nobody reads the plan: it
// re-plans the pair and refuses one that is not clean (PlanProblems), and one
// that is not the env's newest waiting release. It checks env.write per pair
// and is not atomic: one job per pair, each outcome on its own.
func (o *Orchestrator) PromoteClean(ctx context.Context, by *Principal, pairs []PromotePair) []PromoteOutcome {
	out := make([]PromoteOutcome, len(pairs))
	for i, pr := range pairs {
		out[i] = PromoteOutcome{Pair: pr}
		out[i].Skip, out[i].Job, out[i].Err = o.promoteClean(ctx, by, pr)
	}
	return out
}

func (o *Orchestrator) promoteClean(ctx context.Context, by *Principal, pr PromotePair) (skip string, j Job, err error) {
	e, err := o.envs.Get(ctx, pr.EnvID)
	if err != nil {
		return "", j, err
	}
	st, err := o.stacks.Get(ctx, e.StackID)
	if err != nil {
		return "", j, err
	}
	if by == nil {
		return "", j, errs.Refusedf("sign in to promote")
	}
	if err := authz.Can(by.Access, "env.write", authz.Resource{OrgID: st.OrgID, StackID: st.ID}); err != nil {
		return "", j, err
	}
	r, err := o.releases.Get(ctx, pr.ReleaseID)
	if err != nil {
		return "", j, err
	}
	if r.StackID != st.ID {
		return "", j, errs.ErrNotFound
	}
	newest, err := o.newestWaiting(ctx, st.ID, e.ID, r.Number)
	if err != nil {
		return "", j, err
	}
	if !newest {
		return SkipNewer, j, nil
	}
	pp, err := o.PlanPromote(ctx, by, e.ID, r.ID)
	if err != nil {
		return "", j, err
	}
	if !pp.CanDeploy || len(PlanProblems(pp.Plan)) > 0 {
		return SkipReview, j, nil
	}
	ok, err := o.mayPlace(ctx, by, e.ID)
	if err != nil {
		return "", j, err
	}
	j, err = o.enqueuePromoteJob(ctx, promoteJob{EnvID: e.ID, ReleaseID: r.ID, MayPlace: ok, Admin: by.Access.Admin, CleanOnly: true})
	return "", j, err
}

// newestWaiting says whether number is the release the env's rung is newest
// waiting on.
func (o *Orchestrator) newestWaiting(ctx context.Context, stackID, envID string, number int) (bool, error) {
	rungs, err := o.graph.Rungs(ctx, stackID)
	if err != nil {
		return false, err
	}
	for _, g := range rungs {
		if g.EnvID == envID {
			return g.Behind && g.Waiting == number, nil
		}
	}
	return false, nil
}

// cleanGuard is the worker's check for a clean-only promote, run on the final
// plan right before it applies: the plan is still clean and the release is
// still the env's newest waiting one (a newer cut after the enqueue refuses).
func (o *Orchestrator) cleanGuard(ctx context.Context, envID, releaseID string) func(*promote.Plan) error {
	return func(pl *promote.Plan) error {
		if len(PlanProblems(pl)) > 0 {
			return errs.Refusedf("%s", SkipReview)
		}
		e, err := o.envs.Get(ctx, envID)
		if err != nil {
			return err
		}
		r, err := o.releases.Get(ctx, releaseID)
		if err != nil {
			return err
		}
		newest, err := o.newestWaiting(ctx, e.StackID, envID, r.Number)
		if err != nil {
			return err
		}
		if !newest {
			return errs.Refusedf("%s", SkipNewer)
		}
		return nil
	}
}
