package canvas_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
)

// ?plan= names a plan: one older than the 20 listed opens as itself, an id
// that does not exist, or is another org's, is a 404, never the newest plan.
func TestOrgConfigPlanParam(t *testing.T) {
	r := newConfigRig(t)
	ctx := context.Background()
	const tab = "/acme/-/drawer?tab=config"
	seed := func(org string, ago time.Duration) string {
		sum := ""
		if ago == 0 {
			sum = "newest-marker"
		}
		p := service.OrgPlan{
			ID: uuid.NewString(), OrgID: org, Status: "rejected", Ticked: []string{},
			Summary: sum, Plan: `{"changes":[]}`, CreatedAt: time.Now().Add(-ago),
		}
		if err := r.owner.env.Store.OrgPlans.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	old := seed(r.owner.org, time.Hour)
	for i := range 20 {
		seed(r.owner.org, time.Duration(i+1)*time.Second)
	}
	seed(r.owner.org, 0)
	if rec := r.owner.do(t, "GET", tab+"&plan="+uuid.NewString(), nil, true); rec.Code != 404 {
		t.Errorf("unknown plan = %d, want 404", rec.Code)
	}
	other := r.owner.env.Org(t, "other")
	if rec := r.owner.do(t, "GET", tab+"&plan="+seed(other, 0), nil, true); rec.Code != 404 {
		t.Errorf("another org's plan = %d, want 404", rec.Code)
	}
	rec := r.owner.do(t, "GET", tab+"&plan="+old, nil, true)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), ">Latest plan<") || !strings.Contains(rec.Body.String(), ">Plan<") ||
		!strings.Contains(rec.Body.String(), "newest-marker") {
		t.Errorf("older plan = %d\n%s", rec.Code, rec.Body)
	}
}
