package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// StackPlanner is the config planner as this package needs it. An interface
// because the implementation (config/stackconf.Planner) imports this package.
type StackPlanner interface {
	Replanner
	// RunAll re-plans the whole stack: the stack-scoped plan plus one for every
	// environment pinned to its own branch. Every plan it produced comes back,
	// stack-scoped first.
	RunAll(ctx context.Context, stack *repo.Stack, sha string) ([]*repo.ConfigPlan, error)
}

// PlanService owns a config plan's life after it exists: re-planning, and the
// approve and reject decisions.
//
// The drift here was in the gates rather than the doing. The API refused to
// plan or approve on a stack with no config repository bound and the panel did
// not, so the panel's "Plan now" on an unbound stack ran a planner that could
// only fail, and reported the failure as a plan. The pending check answered 409
// on one surface and 400 on the other for the identical race — two people
// pressing Apply.
type PlanService struct {
	store repo.Store
	plan  StackPlanner
	queue ApplyQueue
}

func NewPlanService(store repo.Store, plan StackPlanner, queue ApplyQueue) *PlanService {
	return &PlanService{store: store, plan: plan, queue: queue}
}

// Run re-plans a stack. Every plan it made comes back, stack-scoped one first:
// a caller that reports only the first would tell someone "one change" while a
// branch-bound staging environment has a deletion queued.
func (s *PlanService) Run(ctx context.Context, stack *repo.Stack) ([]*repo.ConfigPlan, error) {
	if stack == nil {
		return nil, svcerr.ErrNotFound
	}
	if !stack.ConfigManaged() {
		return nil, svcerr.Conflictf("stack has no config repo bound")
	}
	return s.plan.RunAll(ctx, stack, "")
}

// Approve queues the apply. It returns the work-queue job id.
//
// Queued, not run: an apply clones repositories and builds images, so it
// routinely outlives the request that asked for it, and on the request's
// context it died wherever it had got to — with the write recording the
// failure going through the same dead context and being lost. The row kept the
// previous run's message and stayed pending for ever.
func (s *PlanService) Approve(ctx context.Context, stack *repo.Stack, cp *repo.ConfigPlan) (string, error) {
	if stack == nil || cp == nil {
		return "", svcerr.ErrNotFound
	}
	if err := s.pending(cp); err != nil {
		return "", err
	}
	if s.queue == nil {
		return "", svcerr.ErrUnavailable
	}
	// Force: a person looked at the plan and pressed Apply, which is the
	// approval the per-environment policy was waiting for.
	return s.queue.Apply(ctx, stack, cp, true, "", "")
}

// Reject closes a plan nobody wants.
func (s *PlanService) Reject(ctx context.Context, cp *repo.ConfigPlan) error {
	if cp == nil {
		return svcerr.ErrNotFound
	}
	if err := s.pending(cp); err != nil {
		return err
	}
	if err := s.store.SetConfigPlanStatus(ctx, cp.ID, "rejected"); err != nil {
		return err
	}
	cp.Status = "rejected"
	return nil
}

// pending is the one answer to "has somebody already decided this". A conflict,
// not a bad request: the request was fine, the world moved. The panel used to
// answer 400 here and the API 409, for the same race.
func (s *PlanService) pending(cp *repo.ConfigPlan) error {
	if cp.Status != "pending" {
		return svcerr.Conflictf("plan is not pending (status: %s)", cp.Status)
	}
	return nil
}

// --- reads ---

// Get is one stack config plan by id.
func (s *PlanService) Get(ctx context.Context, id string) (*repo.ConfigPlan, error) {
	cp, err := s.store.GetConfigPlan(ctx, id)
	if err != nil {
		return nil, err
	}
	if cp == nil {
		return nil, svcerr.ErrNotFound
	}
	return cp, nil
}

// ForStack is a stack's most recent config plans, newest first.
func (s *PlanService) ForStack(ctx context.Context, stackID string, limit int) ([]repo.ConfigPlan, error) {
	return s.store.ListConfigPlans(ctx, stackID, limit)
}

// GetOrgPlan is one ORG config plan by id. Org plans are a separate table
// from stack plans and deliberately separate methods here: they are approved
// by a different owner, at a different level, and conflating the two ids
// would let an org plan be approved through a stack plan's route.
func (s *PlanService) GetOrgPlan(ctx context.Context, id string) (*repo.ConfigPlan, error) {
	cp, err := s.store.GetOrgConfigPlan(ctx, id)
	if err != nil {
		return nil, err
	}
	if cp == nil {
		return nil, svcerr.ErrNotFound
	}
	return cp, nil
}

// ForOrg is an organization's most recent org config plans, newest first.
func (s *PlanService) ForOrg(ctx context.Context, orgID string, limit int) ([]repo.ConfigPlan, error) {
	return s.store.ListOrgConfigPlans(ctx, orgID, limit)
}

// SetOrgPlanStatus records an org plan's outcome.
func (s *PlanService) SetOrgPlanStatus(ctx context.Context, id, status string) error {
	return s.store.SetOrgConfigPlanStatus(ctx, id, status)
}

// AwaitingPlan counts an org's stacks with a plan waiting on somebody. The
// canvas badge.
func (s *PlanService) AwaitingPlan(ctx context.Context, orgID string) (int, error) {
	return s.store.CountStacksAwaitingPlan(ctx, orgID)
}

// Latest is a stack's most recent config plan whatever its state, nil when it
// has never been planned. Nil rather than ErrNotFound: "no plan yet" is what
// an unbound stack looks like, and the banner renders it as a state.
func (s *PlanService) Latest(ctx context.Context, stackID string) (*repo.ConfigPlan, error) {
	return s.store.LatestConfigPlan(ctx, stackID)
}

// LatestSettled is the most recent plan that has been applied or rejected —
// the one a "last applied" line names, which the pending one must not stand in
// for.
func (s *PlanService) LatestSettled(ctx context.Context, stackID string) (*repo.ConfigPlan, error) {
	return s.store.LatestSettledConfigPlan(ctx, stackID)
}

// Work is the queued or running job for one plan's apply, nil when the apply
// has not been enqueued. The panel shows the queue position from it.
func (s *PlanService) Work(ctx context.Context, kind, planID string) (*repo.WorkItem, error) {
	return s.store.LatestWorkItem(ctx, kind, planID)
}
