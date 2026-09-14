package components

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
)

// One renderer serves the org plan page, a stack's plan page and step 3 of the
// wizard, so the things that make a plan safe to approve, the disabled button
// on a broken plan, the boxes for values nobody has set, the detail behind a
// create row, are asserted once, here.
func TestPlanBody(t *testing.T) {
	render := func(cfg PlanViewCfg, p *stackconf.Plan) string {
		c := echo.New().NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
		c.Set("csrf", "test-token")
		var buf bytes.Buffer
		require.NoError(t, PlanBody(c, cfg, p).Render(context.Background(), &buf))
		return buf.String()
	}

	base := PlanViewCfg{Summary: "3 changes", Status: "pending",
		ApproveURL: "/p/approve", RejectURL: "/p/reject", InputURL: "/p/inputs"}
	plan := &stackconf.Plan{
		Changes: []stackconf.Change{
			{Kind: "create", Env: "prod", Tile: "web", New: "service", Fields: []stackconf.Field{
				{Name: "image", Value: "nginx"}, {Name: "env", Value: "SESSION_SECRET"},
			}},
			{Kind: "update", Env: "prod", Tile: "web", Field: "port", Old: "80", New: "8080"},
			{Kind: "delete", Env: "prod", Tile: "old"},
			stackconf.InputChange(stackconf.Input{Scope: "stack", Name: "SESSION_SECRET"}),
		},
		Inputs: []stackconf.Input{{Scope: "stack", Name: "SESSION_SECRET", Secret: true}},
	}

	out := render(base, plan)
	require.Contains(t, out, "<details", "a create row opens to show what it creates")
	require.Contains(t, out, "nginx", "the detail is the fields the planner recorded")
	require.Equal(t, 2, strings.Count(out, "<details"),
		"only the create row and the declared value have detail; a page of one-line updates must not grow a column of dead arrows")
	require.Contains(t, out, `name="value"`, "a declared value gets its box inside its own row")
	require.Contains(t, out, "border-rw-danger/40",
		"an unset declared value reads as an error: it is why a tile will not come up")
	require.NotContains(t, out, "Values this config declares", "the separate panel is gone")
	require.Contains(t, out, "SESSION_SECRET")
	require.Contains(t, out, "/p/inputs")
	require.NotContains(t, out, "disabled", "a plan with only warnings is approvable")
	require.Contains(t, out, "hx-confirm", "a delete is confirmed before it happens")

	// Errors are the only blocker. A missing value is an input, not a fault.
	broken := *plan
	broken.Errors = []string{"tile web: no such image"}
	out = render(base, &broken)
	require.Contains(t, out, "disabled", "approve is refused on a plan the server will refuse")
	require.Contains(t, out, "has errors", "and says why, next to the button")

	// Nothing to decide: no buttons, and no boxes either.
	out = render(PlanViewCfg{Summary: "applied", Status: "applied"}, plan)
	require.NotContains(t, out, "Approve", "an applied plan is a record, not a decision")
	require.NotContains(t, out, `name="value"`, "no inputs without somewhere to post them")
}
