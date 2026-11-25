package service

import (
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
)

func TestPlanProblemsAndChangeText(t *testing.T) {
	if PlanProblems(&Plan{Changes: []Change{{Kind: "update", Tile: "api", Field: "port", Old: "80", New: "8080"}}}) != nil {
		t.Error("a clean plan has no problems")
	}
	p := &Plan{
		Blockers: []string{"needs an owner"},
		Changes:  []Change{{Kind: "update", Tile: "api", Impact: "api restarts"}, {Kind: "param", Field: "x", Optional: true, Key: "param:x"}},
	}
	got := PlanProblems(p)
	if want := []string{"needs an owner", "api restarts"}; !slices.Equal(got[:2], want) || len(got) != 3 {
		t.Errorf("problems = %q", got)
	}
}

// PlanProblems agrees with planfile.Plan.AutoOK: a plan that is not AutoOK
// always has a problem to show; a gap beside real changes (which AutoOK lets
// through) is a problem too, worded "not set".
func TestPlanProblemsAgreeWithAutoOK(t *testing.T) {
	gap := planfile.Gap{Kind: "secret", Key: "db.password"}
	change := Change{Kind: "update", Tile: "api", Field: "port"}
	for name, p := range map[string]*Plan{
		"blocker":      {Blockers: []string{"b"}},
		"impact":       {Changes: []Change{{Kind: "update", Impact: "restarts"}}},
		"removal":      {Changes: []Change{{Kind: "delete", Optional: true, Key: "k"}}},
		"gaps only":    {Gaps: []planfile.Gap{gap}},
		"gap + change": {Changes: []Change{change}, Gaps: []planfile.Gap{gap}},
		"clean":        {Changes: []Change{change}},
		"empty":        {},
	} {
		auto := planfile.Plan{Changes: p.Changes, Blockers: p.Blockers, Gaps: p.Gaps}.AutoOK()
		if probs := PlanProblems(p); auto && len(probs) > 0 && name != "gap + change" || !auto && len(probs) == 0 {
			t.Errorf("%s: AutoOK %v, problems %q", name, auto, probs)
		}
	}
	if got := PlanProblems(&Plan{Changes: []Change{change}, Gaps: []planfile.Gap{gap}}); len(got) != 1 || got[0] != "Value db.password is not set" {
		t.Errorf("gap words = %q", got)
	}
}

// The modal's groups: each fact in its own list, a version with Old and New, a
// param or env row only "changed", a gap by key and never a value.
func TestReleaseDetailFill(t *testing.T) {
	var d ReleaseDetail
	d.fill(&Plan{
		Blockers: []string{"needs an owner"},
		Changes: []Change{
			{Kind: "update", Tile: "api", Field: "image", Old: "1.0", New: "1.1", Impact: "api restarts"},
			{Kind: "update", Tile: "api", Field: "env"},
			{Kind: "param", Field: "db.pass", Old: "s3cret", New: "n3w"},
			{Kind: "route", Optional: true, Key: "route:x", Tile: "x.dev"},
		},
		Gaps: []planfile.Gap{{Kind: "secret", Key: "db.token"}},
	})
	if !slices.Equal(d.Blockers, []string{"needs an owner"}) || !slices.Equal(d.Impacts, []string{"api restarts"}) ||
		len(d.Removals) != 1 || !slices.Equal(d.Gaps, []string{"db.token not set"}) {
		t.Errorf("groups = %+v", d)
	}
	want := []ReleaseChange{{"api image", "1.0", "1.1"}, {"api env", "", "changed"}, {"param db.pass", "", "changed"}}
	if !slices.Equal(d.Changes, want) {
		t.Errorf("changes = %+v", d.Changes)
	}
}
