package store

import (
	"context"
	"strings"
	"time"
)

// OrgPlan is a row of org_config_plans: one plan of an org's config file.
type OrgPlan struct {
	ID        string     `db:"id" json:"id"`
	OrgID     string     `db:"org_id" json:"org_id"`
	Commit    string     `db:"commit_sha" json:"commit"`
	Summary   string     `db:"summary" json:"summary"`
	Plan      string     `db:"plan" json:"plan"`
	Status    string     `db:"status" json:"status"`
	Error     string     `db:"error" json:"error"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
	DecidedAt *time.Time `db:"decided_at" json:"decided_at"`
	// Ticked is the removal keys the approver ticked; Confirmed says they
	// confirmed a plan with impact lines. Both are stamped by Approve.
	Ticked    StringList `db:"ticked" json:"ticked"`
	Confirmed bool       `db:"confirmed" json:"confirmed"`
}

// PlanRefs points at the columns a plan's status machine reads and writes,
// so one machine (leaf/orgplan) serves org_config_plans and
// server_config_plans. Scope is the plan's owner for superseding: the org
// id, "" for the server's.
type PlanRefs struct {
	Scope     string
	ID        *string
	Status    *string
	Error     *string
	CreatedAt *time.Time
	DecidedAt **time.Time
	Ticked    *StringList
	Confirmed *bool
}

// Refs is the machine's view of the row.
func (p *OrgPlan) Refs() PlanRefs {
	return PlanRefs{p.OrgID, &p.ID, &p.Status, &p.Error, &p.CreatedAt, &p.DecidedAt, &p.Ticked, &p.Confirmed}
}

type OrgPlanStore interface {
	Create(ctx context.Context, p OrgPlan) error
	Get(ctx context.Context, id string) (OrgPlan, error)
	// ListByOrg is the org's newest plans, newest first, at most limit.
	ListByOrg(ctx context.Context, orgID string, limit int) ([]OrgPlan, error)
	// ListByStatus is the org's plans in any of statuses.
	ListByStatus(ctx context.Context, orgID string, statuses ...string) ([]OrgPlan, error)
	Update(ctx context.Context, p OrgPlan) error
	Delete(ctx context.Context, id string) error
}

var orgPlansT = newTable[OrgPlan]("org_config_plans", nil)

type orgPlans struct{ crud[OrgPlan] }

func (s orgPlans) ListByOrg(ctx context.Context, orgID string, limit int) ([]OrgPlan, error) {
	return s.many(ctx, "org_id = ? ORDER BY created_at DESC LIMIT ?", orgID, limit)
}

func (s orgPlans) ListByStatus(ctx context.Context, orgID string, statuses ...string) ([]OrgPlan, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	args := []any{orgID}
	for _, st := range statuses {
		args = append(args, st)
	}
	return s.many(ctx, "org_id = ? AND status IN (?"+strings.Repeat(", ?", len(statuses)-1)+")", args...)
}
