package render

import (
	"strings"

	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
)

// RelTone is the chip tone of a releases window state.
var RelTone = map[string]string{
	service.StateWaiting:   "accent",
	service.StateDeploying: "warn",
	service.StateRunning:   "success",
	service.StateFailed:    "danger",
}

// RelPlanRow is a config plan as a window row: the org file's on the org
// chart (scope "org config"), the server file's on the cluster chart (no
// scope). href is the plan's review.
func RelPlanRow(r service.WindowRow, scope, href string) comp.RelRow {
	row := comp.RelRow{
		Kind: "plan", ID: r.PlanID, Scope: scope, Message: r.Message,
		State: r.State, Tone: RelTone[r.State],
	}
	if r.Commit != "" {
		row.Commit = r.Commit[:min(7, len(r.Commit))]
	}
	switch r.State {
	case service.StateWaiting:
		row.Link = comp.RelLink{Label: "Review plan", Href: href}
	case service.StateFailed:
		tail, _ := comp.ErrTail(r.Err, 80)
		row.Message = "Plan failed: " + strings.TrimRight(tail, ".")
		row.Link = comp.RelLink{Label: "View details", Href: href}
	}
	return row
}
