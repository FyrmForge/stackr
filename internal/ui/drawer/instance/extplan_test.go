package instance

import (
	"strings"
	"testing"
)

func renderPlan(t *testing.T, v View, p *ExtPlanView) string {
	t.Helper()
	var b strings.Builder
	if err := LiveExtPlan(v, p).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The plan region always draws its box (an event body is never empty); a plan
// with a restart asks for the tick, a blocked one offers no Apply, and a
// read-only viewer sees the plan without Apply.
func TestLiveExtPlan(t *testing.T) {
	v := View{Base: "/o/s/e/-/instances/pg"}
	if got := renderPlan(t, v, nil); !strings.Contains(got, `id="ext-plan"`) || strings.Contains(got, "Apply plan") {
		t.Errorf("no plan: %s", got)
	}
	p := &ExtPlanView{ID: "p1", Lines: []string{"pgvector is built."}, Versions: []string{"pgvector 0.8.5 to 0.8.6"}, Restart: true}
	got := renderPlan(t, v, p)
	for _, want := range []string{"pgvector is built.", "pgvector 0.8.5 to 0.8.6", `name="plan" value="p1"`, `name="confirm"`, "/extensions/apply", "Apply plan"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if got := renderPlan(t, v, &ExtPlanView{ID: "p2"}); strings.Contains(got, `name="confirm"`) {
		t.Errorf("tick asked for a plan that does not restart:\n%s", got)
	}
	if got := renderPlan(t, v, &ExtPlanView{ID: "p3", Blockers: []string{"b"}}); strings.Contains(got, "Apply plan") || !strings.Contains(got, "<li>b</li>") {
		t.Errorf("blocked:\n%s", got)
	}
	ro := v
	ro.ReadOnly = true
	if got := renderPlan(t, ro, p); strings.Contains(got, "Apply plan") {
		t.Errorf("read-only:\n%s", got)
	}
}
