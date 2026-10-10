package components

import (
	"context"
	"strings"
	"testing"
)

func renderPlan(t *testing.T, v PlanReviewView) string {
	t.Helper()
	var b strings.Builder
	if err := PlanReview(v).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

var removal = ChangeView{Kind: "route-delete", Field: "pve.example.com", Optional: true, Key: "route:pve.example.com"}

// A removal row is a checkbox the approver ticks, unticked by default,
// posted as "ticked"; a viewer who cannot approve reads it without one.
func TestPlanReviewRemovalTicks(t *testing.T) {
	v := PlanReviewView{Plan: PlanView{Changes: []ChangeView{removal}}, Approve: "/a", Reject: "/r"}
	body := renderPlan(t, v)
	if !strings.Contains(body, `name="ticked"`) || !strings.Contains(body, `value="route:pve.example.com"`) {
		t.Errorf("no tick for the removal:\n%s", body)
	}
	if strings.Contains(body, "checked") {
		t.Errorf("a removal is ticked by default:\n%s", body)
	}
	if !strings.Contains(body, "<form") || !strings.Contains(body, `hx-post="/a"`) {
		t.Errorf("approve does not post the ticks:\n%s", body)
	}
	v.Approve, v.Reject = "", ""
	body = renderPlan(t, v)
	if strings.Contains(body, `name="ticked"`) || !strings.Contains(body, "pve.example.com") || !strings.Contains(body, "remove?") {
		t.Errorf("a reader sees a tick or loses the row:\n%s", body)
	}
}

// A plan with impact lines shows them highlighted and opens an "are you
// sure?" listing them; yes posts confirm. Without impact, one click.
func TestPlanReviewImpactConfirm(t *testing.T) {
	risky := PlanReviewView{
		Plan: PlanView{Changes: []ChangeView{
			{Kind: "setting", Field: "panel_domain", Old: "a.test", New: "b.test", Impact: "panel moves to b.test; point DNS first"},
			{Kind: "setting", Field: "workers", New: "3"},
		}},
		Approve: "/a",
	}
	body := renderPlan(t, risky)
	for _, want := range []string{
		"Are you sure?", "panel moves to b.test; point DNS first", `hx-trigger="confirmed"`,
		`hx-post="/a"`, `{&#34;confirm&#34;:&#34;1&#34;}`, "<confirm-dialog",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("risky plan lacks %q:\n%s", want, body)
		}
	}
	if n := strings.Count(body, "panel moves to b.test"); n < 2 {
		t.Errorf("the impact line shows %d times, want on its row and in the dialog", n)
	}
	risky.Plan.Changes = risky.Plan.Changes[1:]
	body = renderPlan(t, risky)
	if strings.Contains(body, "<confirm-dialog") || strings.Contains(body, "Are you sure?") || !strings.Contains(body, `hx-post="/a"`) {
		t.Errorf("a plan with no impact asks anyway or has no approve:\n%s", body)
	}
}

// A blocked plan has no approve; a local plan says so.
func TestPlanReviewBlockedAndOrigin(t *testing.T) {
	body := renderPlan(t, PlanReviewView{
		Plan:   PlanView{Blockers: []string{"dest offsite needs a key"}},
		Reject: "/r",
		Origin: "From a local file, not the repo.",
	})
	if strings.Contains(body, "Approve") || !strings.Contains(body, `hx-post="/r"`) || !strings.Contains(body, "dest offsite needs a key") {
		t.Errorf("blocked plan:\n%s", body)
	}
	if !strings.Contains(body, "From a local file, not the repo.") {
		t.Errorf("no origin line:\n%s", body)
	}
}

// An applied plan shows what the approver ticked: "removed" for a ticked
// removal, "kept" for the rest, no checkbox and no "Applying" line.
func TestPlanReviewDoneShowsTicks(t *testing.T) {
	other := ChangeView{Kind: "route-delete", Field: "b.example.com", Optional: true, Key: "route:b.example.com"}
	body := renderPlan(t, PlanReviewView{
		Plan:   PlanView{Changes: []ChangeView{removal, other}},
		Done:   true,
		Ticked: []string{removal.Key},
	})
	if !strings.Contains(body, ">removed<") || !strings.Contains(body, ">kept<") {
		t.Errorf("no removed/kept chips:\n%s", body)
	}
	for _, bad := range []string{"remove?", `name="ticked"`, "Applying", "removing"} {
		if strings.Contains(body, bad) {
			t.Errorf("done plan shows %q:\n%s", bad, body)
		}
	}
}

// A lock row carries its own Apply button; other rows do not.
func TestPlanApplyLock(t *testing.T) {
	var sb strings.Builder
	p := PlanView{Changes: []ChangeView{{Kind: "lock", Tile: "demo", Old: "locked", New: "unlocked", Apply: "/x/lock", Vals: `{"locked":""}`}, {Kind: "param", Field: "a"}}}
	if err := Plan(p).Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	if strings.Count(sb.String(), "Apply lock change") != 1 || !strings.Contains(sb.String(), `hx-post="/x/lock"`) {
		t.Errorf("plan:\n%s", sb.String())
	}
}
