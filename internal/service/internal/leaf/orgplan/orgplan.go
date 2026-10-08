// Package orgplan owns org_config_plans and server_config_plans: one row per
// plan of an org's or the server's config file and its status machine. A
// plan is stored pending (changes to apply), clean (nothing to do) or error
// (the file did not parse). An approve stamps decided_at, the approver's
// ticks and their confirm on a pending row, which stays pending until the
// apply job marks it applied or error; reject ends it rejected. Build the
// leaf on a store.Tx's table so Create's supersede and insert land together.
//
// The machine is one generic type over the row; Leaf is the org's, Server
// the server's, each adding only the name of its scope.
package orgplan

import (
	"context"
	"slices"
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

// Rows is the table under a Machine; scope is the org id, "" for the
// server's single scope.
type Rows[T any] interface {
	Create(ctx context.Context, p T) error
	Get(ctx context.Context, id string) (T, error)
	Update(ctx context.Context, p T) error
	Newest(ctx context.Context, scope string, limit int) ([]T, error)
	ByStatus(ctx context.Context, scope string, statuses ...string) ([]T, error)
}

// Machine is the status machine over a row type T (store.OrgPlan or
// store.ServerPlan); P is *T with the Refs the machine works through.
type Machine[T any, P interface {
	*T
	Refs() store.PlanRefs
}] struct {
	plans Rows[T]
}

func (m *Machine[T, P]) refs(p *T) store.PlanRefs { return P(p).Refs() }

func (m *Machine[T, P]) Get(ctx context.Context, id string) (T, error) {
	return m.plans.Get(ctx, id)
}

// Newest is the scope's newest plans, newest first, at most limit.
func (m *Machine[T, P]) Newest(ctx context.Context, scope string, limit int) ([]T, error) {
	return m.plans.Newest(ctx, scope, limit)
}

// Create stores a plan (status pending, clean or error) and supersedes the
// scope's older pending and clean rows. An approved row is left to its apply
// job.
func (m *Machine[T, P]) Create(ctx context.Context, p T) (T, error) {
	r := m.refs(&p)
	switch *r.Status {
	case Pending, Clean, Error:
	default:
		return p, errs.Invalidf("status", "A new plan is pending, clean or error, not %q.", *r.Status)
	}
	old, err := m.plans.ByStatus(ctx, r.Scope, Pending, Clean)
	if err != nil {
		return p, err
	}
	for _, o := range old {
		or := m.refs(&o)
		if *or.DecidedAt != nil {
			continue
		}
		*or.Status = Superseded
		if err := m.plans.Update(ctx, o); err != nil {
			return p, err
		}
	}
	*r.ID = uuid.NewString()
	*r.CreatedAt = time.Now().UTC()
	*r.DecidedAt = nil
	*r.Ticked, *r.Confirmed = store.StringList{}, false
	return p, m.plans.Create(ctx, p)
}

// Approve stamps the approver's go on a pending plan, with the removal keys
// they ticked and whether they confirmed its impact lines (the service has
// checked both against the plan). The plan stays pending until the apply job
// ends it. One apply per plan: a second approve is a Conflict.
// ponytail: read then write; two approves in the same instant both pass.
// A conditional UPDATE (invites' Burn) if that ever bites.
func (m *Machine[T, P]) Approve(ctx context.Context, id string, ticked []string, confirmed bool) (T, error) {
	p, err := m.plans.Get(ctx, id)
	if err != nil {
		return p, err
	}
	r := m.refs(&p)
	if err := undecided(*r.Status, *r.DecidedAt); err != nil {
		return p, err
	}
	now := time.Now().UTC()
	*r.DecidedAt = &now
	*r.Ticked, *r.Confirmed = store.StringList(slices.Clone(ticked)), confirmed
	if *r.Ticked == nil {
		*r.Ticked = store.StringList{}
	}
	return p, m.plans.Update(ctx, p)
}

// SetStatus ends a plan: rejected only from pending and not yet approved,
// applied only from pending and approved. Anything else is a Conflict.
func (m *Machine[T, P]) SetStatus(ctx context.Context, id, status string) (T, error) {
	p, err := m.plans.Get(ctx, id)
	if err != nil {
		return p, err
	}
	r := m.refs(&p)
	switch status {
	case Rejected:
		if err := undecided(*r.Status, *r.DecidedAt); err != nil {
			return p, err
		}
		now := time.Now().UTC()
		*r.DecidedAt = &now
	case Applied:
		if *r.Status != Pending || *r.DecidedAt == nil {
			return p, errs.Conflictf("Only an approved plan can be marked applied; this one is %s.", *r.Status)
		}
	default:
		return p, errs.Invalidf("status", "A plan is set rejected or applied, not %q.", status)
	}
	*r.Status = status
	return p, m.plans.Update(ctx, p)
}

// SetError ends a pending plan as error with msg: its apply failed. What
// applied stays; the next plan shows what is left.
func (m *Machine[T, P]) SetError(ctx context.Context, id, msg string) (T, error) {
	p, err := m.plans.Get(ctx, id)
	if err != nil {
		return p, err
	}
	r := m.refs(&p)
	if *r.Status != Pending {
		return p, errs.Conflictf("Only a pending plan can fail; this one is %s.", *r.Status)
	}
	*r.Status, *r.Error = Error, msg
	return p, m.plans.Update(ctx, p)
}

// RejectPending rejects the scope's pending plans nobody approved yet: an
// unbind leaves nothing to approve.
func (m *Machine[T, P]) RejectPending(ctx context.Context, scope string) error {
	ps, err := m.plans.ByStatus(ctx, scope, Pending)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, p := range ps {
		r := m.refs(&p)
		if *r.DecidedAt != nil {
			continue
		}
		*r.Status, *r.DecidedAt = Rejected, &now
		if err := m.plans.Update(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func undecided(status string, decided *time.Time) error {
	switch {
	case status != Pending:
		return errs.Conflictf("This plan is %s; only a pending plan can be approved or rejected.", status)
	case decided != nil:
		return errs.Conflictf("This plan is already approved; its apply job is on it.")
	}
	return nil
}

// Leaf is the org's plans.
type Leaf struct {
	*Machine[store.OrgPlan, *store.OrgPlan]
}

func New(plans store.OrgPlanStore) *Leaf {
	return &Leaf{&Machine[store.OrgPlan, *store.OrgPlan]{plans: orgRows{plans}}}
}

// ForOrg is the org's newest plans, newest first, at most limit.
func (l *Leaf) ForOrg(ctx context.Context, orgID string, limit int) ([]store.OrgPlan, error) {
	return l.Newest(ctx, orgID, limit)
}

type orgRows struct{ store.OrgPlanStore }

func (r orgRows) Newest(ctx context.Context, scope string, limit int) ([]store.OrgPlan, error) {
	return r.ListByOrg(ctx, scope, limit)
}

func (r orgRows) ByStatus(ctx context.Context, scope string, statuses ...string) ([]store.OrgPlan, error) {
	return r.ListByStatus(ctx, scope, statuses...)
}

// Server is the server file's plans.
type Server struct {
	*Machine[store.ServerPlan, *store.ServerPlan]
}

func NewServer(plans store.ServerPlanStore) *Server {
	return &Server{&Machine[store.ServerPlan, *store.ServerPlan]{plans: serverRows{plans}}}
}

// Latest is the newest server plans, newest first, at most limit.
func (s *Server) Latest(ctx context.Context, limit int) ([]store.ServerPlan, error) {
	return s.Newest(ctx, "", limit)
}

// RejectUndecided rejects the pending server plans nobody approved yet.
func (s *Server) RejectUndecided(ctx context.Context) error {
	return s.RejectPending(ctx, "")
}

type serverRows struct{ store.ServerPlanStore }

func (r serverRows) Newest(ctx context.Context, _ string, limit int) ([]store.ServerPlan, error) {
	return r.ListNewest(ctx, limit)
}

func (r serverRows) ByStatus(ctx context.Context, _ string, statuses ...string) ([]store.ServerPlan, error) {
	return r.ListByStatus(ctx, statuses...)
}
