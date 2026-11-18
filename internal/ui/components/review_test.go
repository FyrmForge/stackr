package components

import (
	"context"
	"strings"
	"testing"
)

func renderReview(t *testing.T, v ReviewView) string {
	t.Helper()
	var b strings.Builder
	if err := Review(v).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

var sampleRows = PlanView{Changes: []ChangeView{
	{Kind: "create", Tile: "pg", New: "operated", Details: []DetailView{{"Engine", "postgres"}, {"Major", "18"}, {"Volumes", "pgdata, new"}}},
	{Kind: "update", Tile: "web", Field: "image", Old: "1.4", New: "1.5"},
	{Kind: "create", Tile: "bazarr", New: "service", Details: []DetailView{{"Image", "shop-bazarr:2"}}},
	{Kind: "delete", Tile: "old"},
}}

// The review reads as one flat card: counts, rows with their summary, the
// first create row open, and the one button in the footer.
func TestReviewRows(t *testing.T) {
	if a, c, r := PlanCounts(sampleRows.Changes); a != 2 || c != 1 || r != 1 {
		t.Errorf("counts = %d %d %d", a, c, r)
	}
	body := renderReview(t, ReviewView{Back: "/b", Title: "Release 12 for prod", Chip: "Waiting", Tone: "accent", Plan: sampleRows, Button: "Promote to prod", Action: "/go"})
	for _, want := range []string{"2 to add", "1 to change", "1 to remove", "postgres 18", "image 1.4 to 1.5", "service, shop-bazarr:2", "pgdata, new",
		`<details class="group" open>`, `hx-post="/go"`, ">Promote to prod<", "Nothing is deployed until you press it.", ">Waiting<", `hx-get="/b"`} {
		if !strings.Contains(body, want) {
			t.Errorf("review lacks %q:\n%s", want, body)
		}
	}
	if strings.Count(body, `class="panel`) != 1 || strings.Contains(body, "<confirm-dialog") {
		t.Errorf("not one flat card:\n%s", body)
	}
	if strings.Contains(body, `every 3s`) {
		t.Errorf("an idle review polls:\n%s", body)
	}
}

// An admin's promote approves the access in their name; anyone else is told
// a server admin must. Neither stops the button.
func TestReviewAccess(t *testing.T) {
	a := &ReviewAccess{Tiles: []AccessTile{{"bazarr", []string{"share media@192.168.1.102", "device /dev/dri"}}, {"radarr", []string{"share media@192.168.1.102"}}}}
	v := ReviewView{Plan: sampleRows, Button: "Promote to prod", Action: "/go", Access: a}
	body := renderReview(t, v)
	for _, want := range []string{"2 tiles need elevated access", "A server admin must approve. Promote deploys the rest.", "device /dev/dri", `hx-post="/go"`} {
		if !strings.Contains(body, want) {
			t.Errorf("member lacks %q:\n%s", want, body)
		}
	}
	a.Admin = true
	if body = renderReview(t, v); !strings.Contains(body, "2 tiles get elevated access") || !strings.Contains(body, "Promoting approves this in your name.") {
		t.Errorf("admin:\n%s", body)
	}
	if strings.Contains(body, "disabled") {
		t.Errorf("access stops the button:\n%s", body)
	}
}

// A missing secret is a hard block fixed in place: the button is off with
// the reason, the value is a password field that posts to the re-plan.
func TestReviewMissingSecret(t *testing.T) {
	p := sampleRows
	p.Gaps = []GapView{{Kind: "secret", Label: "gate.password", Field: "gate.password", Post: "/fill"}}
	body := renderReview(t, ReviewView{Plan: p, Button: "Promote to prod", Action: "/go"})
	for _, want := range []string{"Cannot promote yet", "One secret is missing.", `type="password"`, `hx-post="/fill"`, "Save and re-plan", "disabled", "Set gate.password first."} {
		if !strings.Contains(body, want) {
			t.Errorf("review lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `hx-post="/go"`) {
		t.Errorf("a blocked review can promote:\n%s", body)
	}
}

// A running deploy blocks, names its release and its log, and the view
// follows it.
func TestReviewRunning(t *testing.T) {
	body := renderReview(t, ReviewView{Plan: sampleRows, Button: "Promote to prod", Action: "/go", Live: "/live",
		Running: &ReviewRunning{Text: "Release 11 is still deploying to prod.", Href: "/jobs/1"}})
	for _, want := range []string{"Release 11 is still deploying to prod.", `href="/jobs/1"`, "This page updates by itself.", `hx-get="/live"`, "disabled"} {
		if !strings.Contains(body, want) {
			t.Errorf("review lacks %q:\n%s", want, body)
		}
	}
}

// A rollback is the same view with its own button, in danger.
func TestReviewRollback(t *testing.T) {
	body := renderReview(t, ReviewView{Plan: sampleRows, Title: "Roll back prod to release 10", Chip: "Rollback", Tone: "warn", Button: "Roll back prod", Danger: true, Action: "/go"})
	if !strings.Contains(body, "btn-danger") || !strings.Contains(body, ">Roll back prod<") || strings.Contains(body, "Promote") {
		t.Errorf("rollback:\n%s", body)
	}
}

// The stop chip is always in the bar, hidden while nothing failed.
func TestStopChip(t *testing.T) {
	var b strings.Builder
	_ = StopChip(false, false).Render(context.Background(), &b)
	if !strings.Contains(b.String(), `id="env-stop-chip"`) || !strings.Contains(b.String(), "hidden") {
		t.Errorf("chip = %s", b.String())
	}
}
