package admin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
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
	g.POST(b+"/gaps/secret", h.fillSecret, a.Require("serverparams.write"))
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
		for _, g := range pl.Gaps {
			box.Review.Plan.Gaps = append(box.Review.Plan.Gaps, gapView(g.Kind, g.Key, g.Tags))
		}
		box.Review.Reject = ui.Base + "/config/plans/" + latest.ID + "/reject"
		if pl.CanApprove() {
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
		When:    p.CreatedAt,
		Commit:  p.Commit[:min(8, len(p.Commit))],
		Source:  p.Source,
		Error:   p.Error,
	}
}

// gapView is one gap of the server plan as the plan draws it. A secret gets
// the field that posts to fillSecret; a node, the prefilled Add node link.
func gapView(kind, key string, tags []string) comp.GapView {
	if kind == "node" {
		label := "Node " + key
		if len(tags) > 0 {
			label += " (" + strings.Join(tags, ", ") + ")"
		}
		q := url.Values{"name": {key}, "tags": {strings.Join(tags, ",")}}
		return comp.GapView{Kind: kind, Label: label, AddNode: nodesBase + "/add?" + q.Encode()}
	}
	return comp.GapView{Kind: kind, Label: "Server secret " + key, Field: "secret." + key, Post: ui.Base + "/config/gaps/secret"}
}

// POST /-/admin/config/gaps/secret (secret.<collection>.<name>): saves the
// value on the server scope, then plans the bound file again so the gap
// closes. A panel with no bound file only saves.
func (h *handler) fillSecret(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	var es []service.ParamEntry
	for _, e := range render.ParamEntries(form) {
		// the plan fills secrets only; an empty value would keep nothing
		if e.Kind == "secret" && e.Value != "" {
			es = append(es, e)
		}
	}
	if len(es) == 0 {
		return h.answer(c, "config", "", extra{}, errs.Invalidf("value", "enter the value"))
	}
	ctx := c.Request().Context()
	if _, err := h.orch.SetParams(ctx, service.ServerParamScope, es); err != nil {
		return h.answer(c, "config", "", extra{}, err)
	}
	if b, err := h.orch.ServerConfigBinding(ctx); err != nil || b.Repo == "" {
		return h.answer(c, "config", "Saved. Plan the file again to close the gap.", extra{}, err)
	}
	p, err := h.orch.PlanServerConfig(ctx)
	if err != nil {
		return h.answer(c, "config", "", extra{}, err)
	}
	return h.answer(c, "config", "Saved and planned: "+p.Summary+".", extra{}, nil)
}
