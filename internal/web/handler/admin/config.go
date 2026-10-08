package admin

import (
	"encoding/json"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// mountConfig is the Config tab's actions: bind, plan now, unbind, and the
// answers to a plan. The route's Require is the gate, as in Mount.
func (h *handler) mountConfig(g *echo.Group, a *middleware.Access) {
	b := ui.Base + "/config"
	bind, approve := a.Require("serverconfig.bind"), a.Require("serverplan.approve")
	unbind := func(c echo.Context) (string, *service.Job, error) {
		_, err := h.orch.BindServerConfig(c.Request().Context(), "", "", "", "", false)
		return "Unbound: the server is managed from the UI.", nil, err
	}
	g.POST(b, h.act("config", func(c echo.Context) (string, *service.Job, error) {
		f := c.FormValue
		if f("connector") == "" { // the select's "(unbind)"
			return unbind(c)
		}
		_, err := h.orch.BindServerConfig(
			c.Request().Context(), f("connector"), f("repo"), f("branch"), f("path"), f("auto") != "",
		)
		return "Config file saved and planned.", nil, err
	}), bind)
	g.POST(b+"/unbind", h.act("config", unbind), bind)
	g.POST(b+"/plan", h.act("config", func(c echo.Context) (string, *service.Job, error) {
		p, err := h.orch.PlanServerConfig(c.Request().Context())
		return "Planned: " + p.Summary + ".", nil, err
	}), bind)
	g.POST(b+"/plans/:plan/approve", h.act("config", func(c echo.Context) (string, *service.Job, error) {
		opts := render.ApproveOpts(c)
		opts.ApproverID = middleware.Principal(c).User.ID // owns the orgs the apply creates
		j, err := h.orch.ApproveServerPlan(c.Request().Context(), c.Param("plan"), opts)
		return "Approved: the apply is queued.", &j, err
	}), approve)
	g.POST(b+"/plans/:plan/reject", h.act("config", func(c echo.Context) (string, *service.Job, error) {
		_, err := h.orch.RejectServerPlan(c.Request().Context(), c.Param("plan"))
		return "Plan rejected.", nil, err
	}), approve)
}

// configTab is the server file: the binding, the latest plan with its
// answers, the plans before it.
func (h *handler) configTab(c echo.Context, job *comp.JobStatusView) (templ.Component, error) {
	ctx := c.Request().Context()
	bd, err := h.orch.ServerConfigBinding(ctx)
	if err != nil {
		return nil, err
	}
	cs, err := h.orch.ServerConnectors(ctx)
	if err != nil {
		return nil, err
	}
	v := ui.ConfigView{
		Base:      ui.Base,
		Export:    "/api/v1/admin/config/export",
		Connector: bd.ConnectorID,
		Repo:      bd.Repo,
		Branch:    bd.Branch,
		Path:      bd.Path,
		Auto:      bd.Auto,
		Job:       job,
	}
	if job != nil {
		job.Refresh = ui.Base + "?tab=config"
	}
	for _, k := range cs {
		v.Connectors = append(v.Connectors, ui.ConnectorOption{Value: k.ID, Label: k.Name + " (" + k.Host + ")"})
	}
	if bd.Repo != "" {
		v.Unbind = comp.ConfirmView{
			Button:  "Unbind",
			Title:   "Unbind the server file?",
			Warning: "The server goes back to being managed from the UI. Plans nobody approved are rejected; nothing applied changes.",
			Action:  ui.Base + "/config/unbind",
			Target:  "#" + comp.DrawerRoot,
		}
	}
	ps, err := h.orch.ServerPlans(ctx, 20)
	if err != nil {
		return nil, err
	}
	rows := make([]comp.PlanRow, len(ps))
	for i, p := range ps {
		rows[i] = planRow(p)
	}
	if len(ps) == 0 {
		return ui.Config(v), nil
	}
	latest := ps[0]
	var pl service.ServerConfigPlan
	if latest.Plan != "" {
		if err := json.Unmarshal([]byte(latest.Plan), &pl); err != nil {
			return nil, err
		}
	}
	box := &ui.ConfigPlan{
		Row: rows[0],
		Review: comp.PlanReviewView{
			Plan:   render.ServerPlanView(pl),
			Origin: render.PlanOrigin(latest.Source),
		},
	}
	switch {
	case latest.Status == "pending" && latest.DecidedAt != nil: // approved; the apply is running
		box.Review.Applying, box.Review.Ticked = true, latest.Ticked
	case latest.Status == "applied":
		box.Review.Done, box.Review.Ticked = true, latest.Ticked
	case latest.Status == "pending":
		box.Review.Reject = ui.Base + "/config/plans/" + latest.ID + "/reject"
		if !pl.Blocked() {
			box.Review.Approve = ui.Base + "/config/plans/" + latest.ID + "/approve"
		}
	}
	v.Latest, v.Plans = box, rows[1:]
	return ui.Config(v), nil
}

func planRow(p service.ServerPlan) comp.PlanRow {
	return comp.PlanRow{
		Status:  p.Status,
		Summary: p.Summary,
		When:    p.CreatedAt.Local().Format("Jan 2 15:04"),
		Commit:  p.Commit[:min(8, len(p.Commit))],
		Source:  p.Source,
		Error:   p.Error,
	}
}
