package store

import (
	"context"
	"strings"
	"time"
)

// ServerPlan is a row of server_config_plans: one plan of the server file,
// from the bound repo (Source "repo") or a local file (Source "local", File
// holds its bytes). Same shape as OrgPlan without the org.
type ServerPlan struct {
	ID        string     `db:"id" json:"id"`
	Commit    string     `db:"commit_sha" json:"commit"`
	Summary   string     `db:"summary" json:"summary"`
	Plan      string     `db:"plan" json:"plan"`
	Status    string     `db:"status" json:"status"`
	Error     string     `db:"error" json:"error"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
	DecidedAt *time.Time `db:"decided_at" json:"decided_at"`
	Source    string     `db:"source" json:"source"`
	File      string     `db:"file" json:"-"`
	Ticked    StringList `db:"ticked" json:"ticked"`
	Confirmed bool       `db:"confirmed" json:"confirmed"`
}

// Refs is the machine's view of the row; the server has one scope, "".
func (p *ServerPlan) Refs() PlanRefs {
	return PlanRefs{"", &p.ID, &p.Status, &p.Error, &p.CreatedAt, &p.DecidedAt, &p.Ticked, &p.Confirmed}
}

type ServerPlanStore interface {
	Create(ctx context.Context, p ServerPlan) error
	Get(ctx context.Context, id string) (ServerPlan, error)
	// ListNewest is the newest plans, newest first, at most limit.
	ListNewest(ctx context.Context, limit int) ([]ServerPlan, error)
	// ListByStatus is the plans in any of statuses.
	ListByStatus(ctx context.Context, statuses ...string) ([]ServerPlan, error)
	Update(ctx context.Context, p ServerPlan) error
	Delete(ctx context.Context, id string) error
}

var serverPlansT = newTable[ServerPlan]("server_config_plans", nil)

type serverPlans struct{ crud[ServerPlan] }

func (s serverPlans) ListNewest(ctx context.Context, limit int) ([]ServerPlan, error) {
	return s.many(ctx, "1 = 1 ORDER BY created_at DESC LIMIT ?", limit)
}

func (s serverPlans) ListByStatus(ctx context.Context, statuses ...string) ([]ServerPlan, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	args := make([]any, len(statuses))
	for i, st := range statuses {
		args[i] = st
	}
	return s.many(ctx, "status IN (?"+strings.Repeat(", ?", len(statuses)-1)+")", args...)
}
