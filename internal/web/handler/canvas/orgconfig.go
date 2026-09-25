package canvas

import (
	"encoding/json"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	orgui "github.com/FyrmForge/stackr/internal/ui/drawer/org"
	ui "github.com/FyrmForge/stackr/internal/ui/graph"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// configTab is the org's config file: the binding for everyone to read,
// the plans for owners (org.owner.read, as the API), the answers for
// orgplan.approve holders.
func (h *handler) configTab(c echo.Context, cd card, f *comp.DrawerView) (templ.Component, error) {
	ctx, og := c.Request().Context(), cd.s.Org
	cs, err := h.orch.Connectors(ctx, og.ID)
	if err != nil {
		return nil, err
	}
	v := orgui.ConfigView{
		Export:    "/api/v1/orgs/" + og.Slug + "/config/export",
		Connector: og.ConfigConnectorID,
		Repo:      og.ConfigRepo,
		Branch:    og.ConfigBranch,
		Path:      og.ConfigPath,
		Auto:      og.ConfigAuto,
	}
	for _, k := range cs {
		v.Connectors = append(v.Connectors, orgui.Option{Value: k.ID, Label: k.Name + " (" + k.Host + ")"})
	}
	if can(c, cd.s, "org.config.bind") {
		v.Base = f.Base
		if og.ConfigRepo != "" {
			v.Unbind = comp.ConfirmView{
				Button:  "Unbind",
				Title:   "Unbind the config file?",
				Warning: "The organization goes back to being managed from the UI. Plans nobody approved are rejected; nothing applied changes.",
				Action:  f.Base + "/unbind",
				Target:  "#" + comp.DrawerRoot,
			}
		}
	}
	if q, ok := c.Get(jobKey).(queued); ok {
		jv := render.JobView(q.page, q.job)
		jv.Refresh = f.Base + "?tab=config"
		v.Job = &jv
	}
	if !can(c, cd.s, "org.owner.read") {
		return orgui.Config(v), nil
	}
	ps, err := h.orch.OrgPlans(ctx, og.ID, 20)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		v.Plans = append(v.Plans, planRow(p))
	}
	if len(ps) == 0 {
		return orgui.Config(v), nil
	}
	latest := ps[0]
	var pl service.OrgConfigPlan
	if latest.Plan != "" {
		if err := json.Unmarshal([]byte(latest.Plan), &pl); err != nil {
			return nil, err
		}
	}
	box := &orgui.PlanBox{
		Row: v.Plans[0],
		Ask: comp.PromoteAsk{Target: og.Name, Plan: orgPlanView(pl)},
	}
	if latest.Status == "pending" && can(c, cd.s, "orgplan.approve") {
		box.Reject = f.Base + "/plans/" + latest.ID + "/reject"
		if !pl.Blocked() {
			box.Approve = f.Base + "/plans/" + latest.ID + "/approve"
		}
	}
	v.Latest, v.Plans = box, v.Plans[1:]
	return orgui.Config(v), nil
}

func planRow(p service.OrgPlan) orgui.PlanRow {
	return orgui.PlanRow{
		Status:  p.Status,
		Summary: p.Summary,
		When:    p.CreatedAt.Local().Format("Jan 2 15:04"),
		Commit:  p.Commit[:min(8, len(p.Commit))],
		Error:   p.Error,
	}
}

// orgPlanView is planView for the org file's plan: its notes read as
// warnings do.
func orgPlanView(p service.OrgConfigPlan) comp.PlanView {
	pv := comp.PlanView{
		Title:     "What changes",
		Blockers:  p.Blockers,
		Warnings:  p.Notes,
		CanDeploy: !p.Blocked(),
	}
	for _, ch := range p.Changes {
		pv.Changes = append(pv.Changes, comp.ChangeView{
			Kind:  ch.Kind,
			Tile:  ch.Tile,
			Field: ch.Field,
			Old:   ch.Old,
			New:   ch.New,
			Note:  ch.Note,
		})
	}
	return pv
}

// planBanner is v0's org canvas strip while the latest plan waits
// (pending) or failed (error); nil otherwise, and for anyone who cannot
// read the plans it leads to.
// ponytail: drawn at page load; the canvas stream does not repaint it.
func (h *handler) planBanner(c echo.Context) (*ui.Banner, error) {
	s := middleware.ScopeOf(c)
	if s.Org == nil || s.Stack != nil || !can(c, s, "org.owner.read") {
		return nil, nil
	}
	ps, err := h.orch.OrgPlans(c.Request().Context(), s.Org.ID, 1)
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	href := "/" + s.Org.Slug + "?drawer=org:" + s.Org.ID + "&tab=config"
	switch ps[0].Status {
	case "pending":
		return &ui.Banner{Text: "Config plan pending: " + ps[0].Summary + ". View", Href: href}, nil
	case "error":
		return &ui.Banner{Text: "Config invalid. View details", Href: href, Danger: true}, nil
	}
	return nil, nil
}

func (h *handler) mountOrgConfig(site *echo.Group, a *middleware.Access) {
	o := "/:org/-/drawer"
	bind, approve := a.Require("org.config.bind"), a.Require("orgplan.approve")
	unbind := func(c echo.Context, og *service.Org) (string, error) {
		_, err := h.orch.SetOrgConfigRepo(c.Request().Context(), og.ID, "", "", "", "", false)
		return "Unbound: the organization is managed from the UI.", err
	}
	site.POST(o+"/config", h.orgAction("config", func(c echo.Context, og *service.Org) (string, error) {
		f := c.FormValue
		if f("connector") == "" { // v0's "(unbind)"
			return unbind(c, og)
		}
		_, err := h.orch.SetOrgConfigRepo(
			c.Request().Context(),
			og.ID,
			f("connector"),
			f("repo"),
			f("branch"),
			f("path"),
			f("auto") != "",
		)
		return "Config file saved and planned.", err
	}), bind)
	site.POST(o+"/unbind", h.orgAction("config", unbind), bind)
	site.POST(o+"/plan", h.orgAction("config", func(c echo.Context, og *service.Org) (string, error) {
		p, err := h.orch.PlanOrgConfig(c.Request().Context(), og.ID)
		return "Planned: " + p.Summary + ".", err
	}), bind)
	site.POST(o+"/plans/:plan/approve", h.orgAction("config", func(c echo.Context, og *service.Org) (string, error) {
		j, err := h.orch.ApproveOrgPlan(c.Request().Context(), c.Param("plan"))
		if err != nil {
			return "", err
		}
		c.Set(jobKey, queued{job: j, page: "/" + og.Slug})
		return "Approved: the apply is queued.", nil
	}), approve)
	site.POST(o+"/plans/:plan/reject", h.orgAction("config", func(c echo.Context, _ *service.Org) (string, error) {
		_, err := h.orch.RejectOrgPlan(c.Request().Context(), c.Param("plan"))
		return "Plan rejected.", err
	}), approve)
	// the apply's status stream, as an env page has its jobs'
	site.GET("/:org/-/jobs/:job/events", func(c echo.Context) error {
		return render.JobStream(c, h.orch, "/"+middleware.ScopeOf(c).Org.Slug)
	}, a.Require("deployment.read"))
}
