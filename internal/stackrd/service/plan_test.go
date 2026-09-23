package service

import (
	"context"
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type planStore struct {
	repo.Store
	statuses map[string]string
}

func (p *planStore) SetConfigPlanStatus(_ context.Context, id, status string) error {
	if p.statuses == nil {
		p.statuses = map[string]string{}
	}
	p.statuses[id] = status
	return nil
}

// The panel planned an unbound stack, which ran a planner that could only
// fail and reported the failure as a plan. The API refused. One answer now.
func TestRunRefusesAnUnboundStack(t *testing.T) {
	svc := NewPlanService(&planStore{}, nil, nil)
	_, err := svc.Run(context.Background(), &repo.Stack{ID: "s1"})
	var conflict svcerr.Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("an unbound stack should be a conflict, got %v", err)
	}
}

// Two people pressing Apply is a race, not a bad request. The panel answered
// 400 and the API 409 for the identical situation.
func TestApproveAndRejectNeedAPendingPlan(t *testing.T) {
	ctx := context.Background()
	st := &planStore{}
	q := &fakeQueue{}
	svc := NewPlanService(st, nil, q)
	stack := &repo.Stack{ID: "s1"}

	var conflict svcerr.Conflict
	_, err := svc.Approve(ctx, stack, &repo.ConfigPlan{ID: "p1", Status: "applied"})
	if !errors.As(err, &conflict) {
		t.Fatalf("approving a decided plan should conflict, got %v", err)
	}
	if err := svc.Reject(ctx, &repo.ConfigPlan{ID: "p1", Status: "rejected"}); !errors.As(err, &conflict) {
		t.Fatalf("rejecting a decided plan should conflict, got %v", err)
	}

	cp := &repo.ConfigPlan{ID: "p2", Status: "pending"}
	if _, err := svc.Approve(ctx, stack, cp); err != nil {
		t.Fatalf("a pending plan was refused: %v", err)
	}
	// Force, because a person read the plan and pressed Apply.
	if len(q.applied) != 1 || q.applied[0] != "p2//" {
		t.Fatalf("applied %v", q.applied)
	}
	if err := svc.Reject(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if st.statuses["p2"] != "rejected" || cp.Status != "rejected" {
		t.Fatalf("reject wrote %v / %q", st.statuses, cp.Status)
	}
}
