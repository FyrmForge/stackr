package components

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func reviewHTML(t *testing.T, v ReviewView) string {
	t.Helper()
	var b strings.Builder
	if err := Review(v).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// A change row says which settings change on its closed line and opens to a
// table of now and after; a blocker carries its fix link and the footer says
// what to do first; a skipped release is named with a link to its review.
func TestReviewChangeRowsAndBlockers(t *testing.T) {
	ch := ChangeView{Kind: "domain", Tile: "bazarr", New: "bazarr.vulpe.dev", Note: "basic_auth removed, forward_auth added",
		Details: []DetailView{{"basic_auth", "removed"}, {"forward_auth", "added https://auth.vulpe.dev/api/authz"}, {"port", "80 to 81"}}}
	b := "tile authelia: auth.vulpe.dev is an external route."
	v := ReviewView{
		Title: "Release 1 for prod", When: time.Now(), Back: "/b", Button: "Promote to prod", Action: "/go",
		Plan: PlanView{Changes: []ChangeView{ch, {Kind: "update", Tile: "x", Field: "image", Impact: "restarts x"}}, Blockers: []string{b},
			Fix: map[string]BlockerFix{b: {"Open server routes", "/-/admin/traffic", "Remove the route first."}}},
		Skip: &ReviewSkip{"Release 3 (b653b0c) is newer config and is not in this release.", "Review release 3", "/r3"},
	}
	body := reviewHTML(t, v)
	for _, want := range []string{
		"bazarr.vulpe.dev basic_auth removed, forward_auth added", ">Setting<", ">Now<", ">After<",
		">not set<", ">80<", ">81<", "Open server routes", `href="/-/admin/traffic"`, "Remove the route first.",
		"Release 3 (b653b0c) is newer config", ">Review release 3<", ">needs care<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("review lacks %q", want)
		}
	}
	if strings.Contains(body, "border-l-2") {
		t.Error("an impact row still wears a stripe")
	}
	if os.Getenv("REL_DUMP") != "" {
		_ = os.WriteFile(os.Getenv("REL_DUMP")+".review", []byte(body), 0o600)
	}
}

func TestChangeCells(t *testing.T) {
	for in, want := range map[string][2]string{"removed": {"set", "not set"}, "added x y": {"not set", "x y"}, "80 to 81": {"80", "81"}, "changed": {"", "changed"}} {
		if a, b := changeCells(in); a != want[0] || b != want[1] {
			t.Errorf("%q = %q, %q", in, a, b)
		}
	}
}
