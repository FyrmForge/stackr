package admin_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// An approved plan stays "pending" until its apply ends; the tab then shows
// it as applying, with the ticks the approver made and no answers to give.
func TestAdminConfigApplyingPlan(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	p := serverPlan(t, s, "repo", riskyServerPlan)
	// what Approve stamps; seeded, so a fast apply cannot end the plan first
	now := time.Now()
	p.DecidedAt, p.Ticked, p.Confirmed = &now, []string{"route:pve.example.com"}, true
	if err := s.Store.ServerPlans.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	body := s.As(t, root, "GET", "/-/admin?tab=config", nil).Body.String()
	for _, gone := range []string{"/approve", "/reject", `name="ticked"`, "Are you sure?"} {
		if strings.Contains(body, gone) {
			t.Errorf("an applying plan still offers %q:\n%s", gone, body)
		}
	}
	for _, want := range []string{"Applying", "removing"} {
		if !strings.Contains(body, want) {
			t.Errorf("applying view lacks %q:\n%s", want, body)
		}
	}
}

// Once the apply ended, the tab still shows the approver's ticks: the
// ticked removal as removed, the unticked as kept, no "remove?".
func TestAdminConfigAppliedPlanShowsTicks(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	p := serverPlan(t, s, "repo", riskyServerPlan)
	now := time.Now()
	p.DecidedAt, p.Ticked, p.Confirmed, p.Status = &now, []string{"route:pve.example.com"}, true, "applied"
	if err := s.Store.ServerPlans.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	body := s.As(t, root, "GET", "/-/admin?tab=config", nil).Body.String()
	if !strings.Contains(body, ">removed<") || strings.Contains(body, "remove?") || strings.Contains(body, "Applying") {
		t.Errorf("applied plan does not show its ticks:\n%s", body)
	}
}
