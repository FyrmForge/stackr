package render

import (
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/components"
)

// ServerPlanView is the server file's plan as the plan component draws it,
// the way OrgPlanView draws the org's: a row that adds something is a
// create naming its noun and the name it makes. The admin drawer's Config tab shows it.
func ServerPlanView(p service.ServerConfigPlan) components.PlanView {
	pv := components.PlanView{
		Title:     "What changes",
		Blockers:  p.Blockers,
		Warnings:  p.Notes,
		CanDeploy: !p.Blocked(),
	}
	for _, ch := range p.Changes {
		cv := components.ChangeView{
			Kind:     ch.Kind,
			Tile:     ch.Tile,
			Field:    ch.Field,
			Old:      ch.Old,
			New:      ch.New,
			Note:     ch.Note,
			Impact:   ch.Impact,
			Key:      ch.Key,
			Optional: ch.Optional,
		}
		switch ch.Kind {
		case "org-create", "route", "dest", "domain", "param", "connector-share":
			if !ch.Optional {
				cv.Kind = "create"
				cv.Tile = strings.TrimSpace(strings.TrimSuffix(ch.Kind, "-create") + " " + ch.Tile)
			}
		}
		pv.Changes = append(pv.Changes, cv)
	}
	return pv
}

// PlanOrigin is the line under a plan that did not come from the bound
// repo; "" for one that did.
func PlanOrigin(source string) string {
	if source == "local" {
		return "From a local file, not the repo."
	}
	return ""
}

// ApproveOpts reads an approve form: ticked is the removal keys (one field
// each), confirm is set once the approver said yes to the impact lines.
func ApproveOpts(c echo.Context) service.ApproveOpts {
	vals, _ := c.FormParams()
	return service.ApproveOpts{Ticked: vals["ticked"], Confirm: c.FormValue("confirm") != ""}
}
