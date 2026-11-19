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
	if strings.Contains(body, " checked") {
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

// Once applied, rows read in the past tense; the header says what was done.
func TestPlanDonePastTense(t *testing.T) {
	v := PlanReviewView{Done: true, Ticked: []string{"route:a"}, Plan: PlanView{Changes: []ChangeView{
		{Kind: "create", Tile: "route x"},
		{Kind: "route-update", Field: "f"},
		{Kind: "route-delete", Field: "a", Key: "route:a", Optional: true},
		{Kind: "route-delete", Field: "b", Key: "route:b", Optional: true},
	}}}
	body := renderPlan(t, v)
	for _, want := range []string{">created<", ">route-updated<"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in\n%s", want, body)
		}
	}
	if got := AppliedSummary(v); got != "2 applied, 1 removed, 1 kept" {
		t.Errorf("summary = %q", got)
	}
	if got := AppliedSummary(PlanReviewView{Plan: PlanView{Changes: []ChangeView{removal}}}); got != "nothing changed, 1 kept" {
		t.Errorf("removal-only summary = %q", got)
	}
}

// The removal tick is a visible control, not a bare checkbox.
func TestPlanTickIsAControl(t *testing.T) {
	body := renderPlan(t, PlanReviewView{Plan: PlanView{Changes: []ChangeView{removal}}, Approve: "/a"})
	if !strings.Contains(body, "border-rw-warn/60 bg-rw-warn/10") || !strings.Contains(body, "h-5 w-5") {
		t.Errorf("tick not a control:\n%s", body)
	}
}

// A create row opens to what it makes, and the new tile's own settings rows
// fold under it instead of standing as rows of their own.
func TestPlanCreateRowDetails(t *testing.T) {
	v := PlanReviewView{Plan: PlanView{Changes: []ChangeView{
		{Kind: "create", Tile: "pg", New: "operated", Details: []DetailView{{"Engine", "postgres"}, {"Major", "18"}}},
		{Kind: "operated", Tile: "pg", Field: "pitr_days", New: "7"},
		{Kind: "operated", Tile: "web", Field: "allow", New: "+x"},
	}}}
	body := renderPlan(t, v)
	for _, want := range []string{"What it makes", ">Engine<", ">postgres<", "Settings of the new tile", ">pitr_days<"} {
		if !strings.Contains(body, want) {
			t.Errorf("create row lacks %q:\n%s", want, body)
		}
	}
	if n := strings.Count(body, "<li class=\"py-2.5"); n != 2 {
		t.Errorf("rows = %d, want the create (settings inside) and web's own", n)
	}
}

// A removal-only plan asks before an approve with nothing ticked; one with a
// plain row too does not.
func TestPlanReviewRemovalOnlyPrompt(t *testing.T) {
	body := renderPlan(t, PlanReviewView{Plan: PlanView{Changes: []ChangeView{removal}}, Approve: "/a"})
	for _, want := range []string{
		"Nothing will change. Tick the removal to apply it.", "Keep it and close the plan", "Go back",
		`{&#34;keep&#34;:&#34;1&#34;,&#34;confirm&#34;:&#34;1&#34;}`, "group-has-[input[name=ticked]:checked]/rm:hidden",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("removal-only plan lacks %q:\n%s", want, body)
		}
	}
	mixed := PlanReviewView{Plan: PlanView{Changes: []ChangeView{removal, {Kind: "setting", Field: "workers", New: "3"}}}, Approve: "/a"}
	if body := renderPlan(t, mixed); strings.Contains(body, "Nothing will change") {
		t.Errorf("a plan with a change asks the removal question:\n%s", body)
	}
}
