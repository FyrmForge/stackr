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
