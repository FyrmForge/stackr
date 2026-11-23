package render

import (
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
)

// RelPlanRow is a config plan as a window row: the org file's on the org
// chart (scope "org config"), the server file's on the cluster chart (no
// scope). href is the plan's review; viewOnly: the viewer cannot approve.
func RelPlanRow(r service.WindowRow, scope, href string, viewOnly bool) comp.ReleaseLineView {
	row := comp.ReleaseLineView{
		Stack: scope, Cause: r.Message, At: r.At,
		State: r.State, Tone: StateTone[r.State], OfStack: false,
		Avatar: comp.Avatar{Icon: "github"}, // the config file lives in git
	}
	if !r.At.IsZero() {
		row.Age = AgoWord(time.Since(r.At))
	}
	if r.Commit != "" {
		row.Commit = ShortSHA(r.Commit)
	}
	switch r.State {
	case service.StateWaiting:
		row.Link = comp.RelLink{Label: "Review plan", Href: href}
		if viewOnly {
			row.Link.Label = "View plan"
		}
	case service.StateFailed:
		tail, _ := comp.ErrTail(r.Err, 80)
		row.Cause = "Plan failed: " + strings.TrimRight(tail, ".")
		row.Link = comp.RelLink{Label: "View details", Href: href}
	}
	return row
}
