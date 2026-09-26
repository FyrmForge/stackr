// Package orgplan owns org_config_plans: one row per plan of an org's config
// file and its status machine. A plan is stored pending (changes to apply),
// clean (nothing to do) or error (the file did not parse). An owner's
// approve stamps decided_at on a pending row, which stays pending until the
// apply job marks it applied or error; reject ends it rejected. Build the
// leaf on a store.Tx's table so Create's supersede and insert land together.
package orgplan

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The six statuses (REWRITE.md "Org config file"). There is no approved
// status: an approved plan is a pending one with decided_at set.
const (
	Pending    = "pending"
	Clean      = "clean"
	Error      = "error"
	Superseded = "superseded"
	Applied    = "applied"
	Rejected   = "rejected"
)

type Leaf struct{ plans store.OrgPlanStore }

func New(plans store.OrgPlanStore) *Leaf {
	return &Leaf{plans: plans}
}

func (l *Leaf) Get(ctx context.Context, id string) (store.OrgPlan, error) {
	return l.plans.Get(ctx, id)
}

// ForOrg is the org's newest plans, newest first, at most limit.
func (l *Leaf) ForOrg(ctx context.Context, orgID string, limit int) ([]store.OrgPlan, error) {
	return l.plans.ListByOrg(ctx, orgID, limit)
}

// Create stores a plan (status pending, clean or error) and supersedes the
// org's older pending and clean rows. An approved row is left to its apply
// job.
func (l *Leaf) Create(ctx context.Context, p store.OrgPlan) (store.OrgPlan, error) {
	switch p.Status {
	case Pending, Clean, Error:
	default:
		return p, errs.Invalidf("status", "A new plan is pending, clean or error, not %q.", p.Status)
	}
	old, err := l.plans.ListByStatus(ctx, p.OrgID, Pending, Clean)
	if err != nil {
		return p, err
	}
	for _, o := range old {
		if o.DecidedAt != nil {
			continue
		}
		o.Status = Superseded
		if err := l.plans.Update(ctx, o); err != nil {
			return p, err
		}
	}
	p.ID = uuid.NewString()
	p.CreatedAt = time.Now().UTC()
	p.DecidedAt = nil
	return p, l.plans.Create(ctx, p)
}

// Approve stamps the owner's go on a pending plan, which stays pending
// until the apply job ends it. One apply per plan: a second approve is a
// Conflict.
// ponytail: read then write; two approves in the same instant both pass.
// A conditional UPDATE (invites' Burn) if that ever bites.
func (l *Leaf) Approve(ctx context.Context, id string) (store.OrgPlan, error) {
	p, err := l.plans.Get(ctx, id)
	if err != nil {
		return p, err
	}
	if err := undecided(p); err != nil {
		return p, err
	}
	now := time.Now().UTC()
	p.DecidedAt = &now
	return p, l.plans.Update(ctx, p)
}

// SetStatus ends a plan: rejected only from pending and not yet approved,
// applied only from pending and approved. Anything else is a Conflict.
func (l *Leaf) SetStatus(ctx context.Context, id, status string) (store.OrgPlan, error) {
	p, err := l.plans.Get(ctx, id)
	if err != nil {
		return p, err
	}
	switch status {
	case Rejected:
		if err := undecided(p); err != nil {
			return p, err
		}
		now := time.Now().UTC()
		p.DecidedAt = &now
	case Applied:
		if p.Status != Pending || p.DecidedAt == nil {
			return p, errs.Conflictf("Only an approved plan can be marked applied; this one is %s.", p.Status)
		}
	default:
		return p, errs.Invalidf("status", "A plan is set rejected or applied, not %q.", status)
	}
	p.Status = status
	return p, l.plans.Update(ctx, p)
}

// SetError ends a pending plan as error with msg: its apply failed. What
// applied stays; the next plan shows what is left.
func (l *Leaf) SetError(ctx context.Context, id, msg string) (store.OrgPlan, error) {
	p, err := l.plans.Get(ctx, id)
	if err != nil {
		return p, err
	}
	if p.Status != Pending {
		return p, errs.Conflictf("Only a pending plan can fail; this one is %s.", p.Status)
	}
	p.Status, p.Error = Error, msg
	return p, l.plans.Update(ctx, p)
}

// RejectPending rejects the org's pending plans nobody approved yet: an
// unbind leaves nothing to approve.
func (l *Leaf) RejectPending(ctx context.Context, orgID string) error {
	ps, err := l.plans.ListByStatus(ctx, orgID, Pending)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, p := range ps {
		if p.DecidedAt != nil {
			continue
		}
		p.Status, p.DecidedAt = Rejected, &now
		if err := l.plans.Update(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func undecided(p store.OrgPlan) error {
	switch {
	case p.Status != Pending:
		return errs.Conflictf("This plan is %s; only a pending plan can be approved or rejected.", p.Status)
	case p.DecidedAt != nil:
		return errs.Conflictf("This plan is already approved; its apply job is on it.")
	}
	return nil
}
