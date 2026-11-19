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
	body := s.As(t, root, "GET", "/-/admin/config", nil).Body.String()
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
	body := s.As(t, root, "GET", "/-/admin/config", nil).Body.String()
	if !strings.Contains(body, ">removed<") || strings.Contains(body, "remove?") || strings.Contains(body, "Applying") {
		t.Errorf("applied plan does not show its ticks:\n%s", body)
	}
}

// An applied removal-only plan with nothing ticked says nothing changed in
// its header, not the "removal to review" it was planned with; the tab polls
// itself so the card never waits for a reload.
func TestAdminConfigAppliedRemovalOnlyHeader(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	p := serverPlan(t, s, "repo", `{"changes":[{"kind":"route-delete","field":"pve.example.com","key":"route:pve.example.com","optional":true}]}`)
	now := time.Now()
	p.DecidedAt, p.Status = &now, "applied"
	if err := s.Store.ServerPlans.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	body := s.As(t, root, "GET", "/-/admin/config", nil).Body.String()
	if strings.Contains(body, "removal to review") || !strings.Contains(body, "nothing changed, 1 kept") {
		t.Errorf("applied header still the planned one:\n%s", body)
	}
	if !strings.Contains(body, `hx-get="/-/admin/config"`) {
		t.Errorf("the config tab does not poll:\n%s", body)
	}
}

// ?plan= names a plan: one older than the 20 listed opens as itself, an id
// that does not exist is a 404, never the newest plan with its Approve.
func TestAdminConfigPlanParam(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	old := serverPlan(t, s, "repo", riskyServerPlan)
	old.Status, old.CreatedAt = "rejected", time.Now().Add(-time.Hour)
	if err := s.Store.ServerPlans.Update(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		serverPlan(t, s, "repo", riskyServerPlan)
	}
	if rec := s.As(t, root, "GET", "/-/admin/config?plan=nope", nil); rec.Code != 404 {
		t.Errorf("unknown plan = %d, want 404", rec.Code)
	}
	rec := s.As(t, root, "GET", "/-/admin/config?plan="+old.ID, nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "/plans/"+old.ID+"/approve") || !strings.Contains(rec.Body.String(), ">Plan<") {
		t.Errorf("older plan = %d\n%s", rec.Code, rec.Body)
	}
}
