package components

import (
	"strings"
	"testing"
)

// A secret gap is an empty password field that posts alone; a node gap is a
// button to the prefilled Add node steps. Neither blocks the approve.
func TestPlanGaps(t *testing.T) {
	v := PlanReviewView{Approve: "/a", Plan: PlanView{Gaps: []GapView{
		{Kind: "secret", Label: "Server secret db.password", Field: "secret.db.password", Post: "/g"},
		{Kind: "node", Label: "Node h4 (ingress)", AddNode: "/-/admin/nodes/add?name=h4"},
	}}}
	body := renderPlan(t, v)
	for _, want := range []string{
		`type="password"`, `name="secret.db.password"`, `hx-post="/g"`, `hx-params="secret.db.password"`,
		"Add this node", `hx-get="/-/admin/nodes/add?name=h4"`, `hx-post="/a"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in\n%s", want, body)
		}
	}
	if strings.Contains(body, "value=") {
		t.Errorf("a gap field carries a value:\n%s", body)
	}
	// a secret gap with no route is a row, not a field
	v.Plan.Gaps = []GapView{{Kind: "secret", Label: "Org secret x.y"}}
	if body := renderPlan(t, v); strings.Contains(body, `type="password"`) || !strings.Contains(body, "Org secret x.y") {
		t.Errorf("fieldless gap:\n%s", body)
	}
}
