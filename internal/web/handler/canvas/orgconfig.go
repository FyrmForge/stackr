package canvas

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	orgui "github.com/FyrmForge/stackr/internal/ui/drawer/org"
	"github.com/FyrmForge/stackr/internal/web/handler/setup"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// configTab is the org's config file: the binding for everyone to read,
// the plans for owners (org.owner.read, as the API), the answers for
// orgplan.approve holders.
func (h *handler) configTab(c echo.Context, cd card, f *comp.DrawerView) (templ.Component, error) {
	ctx, og := c.Request().Context(), cd.s.Org
	// the picker: the org's own connectors and the server ones shared with it
	cs, err := h.orch.ConfigConnectors(ctx, og.ID)
	if err != nil {
		return nil, err
	}
	v := orgui.ConfigView{
		Export:    "/api/v1/orgs/" + og.Slug + "/config/export",
		Canvas:    "/" + og.Slug,
		Picker:    f.Base + "/config/picker",
		Connector: og.ConfigConnectorID,
		Repo:      og.ConfigRepo,
		Branch:    og.ConfigBranch,
		Path:      og.ConfigPath,
		Auto:      og.ConfigAuto,
	}
	for _, k := range cs {
		label := k.Name + " (" + k.Host + ")"
		if k.OrgID == nil {
			label += ", server"
		}
		v.Connectors = append(v.Connectors, orgui.Option{Value: k.ID, Label: label})
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
	} else if jv, _ := render.ActiveJob(c, h.orch, "org-plan", "org-apply"); jv != nil {
		jv.Refresh = f.Base + "?tab=config"
		v.Job = jv
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
	// ?plan= opens that plan's review (one older than the list too, if it is
	// this org's), else the newest one
	want := c.QueryParam("plan")
	i := slices.IndexFunc(ps, func(p service.OrgPlan) bool { return p.ID == want })
	latest, row := ps[max(i, 0)], v.Plans[max(i, 0)]
	if want != "" && i < 0 {
		if latest, err = h.orch.OrgPlan(ctx, want); err != nil || latest.OrgID != og.ID {
			return nil, errs.ErrNotFound
		}
		row = planRow(latest)
	}
	var pl service.OrgConfigPlan
	if latest.Plan != "" {
		if err := json.Unmarshal([]byte(latest.Plan), &pl); err != nil {
			return nil, err
		}
	}
	box := &orgui.PlanBox{
		Row:    row,
		Review: comp.PlanReviewView{Plan: render.OrgPlanView(pl)},
	}
	switch {
	case latest.Status == "pending" && latest.DecidedAt != nil: // approved; the apply is running
		box.Review.Applying, box.Review.Ticked = true, latest.Ticked
	case latest.Status == "applied":
		box.Review.Done, box.Review.Ticked = true, latest.Ticked
		box.Row.Summary = comp.AppliedSummary(box.Review)
	case latest.Status == "pending" && can(c, cd.s, "orgplan.approve"):
		box.Review.Reject = f.Base + "/plans/" + latest.ID + "/reject"
		if pl.CanApprove() {
			box.Review.Approve = f.Base + "/plans/" + latest.ID + "/approve"
		}
	}
	if latest.Status == "pending" && latest.DecidedAt == nil {
		fill := can(c, cd.s, "org.config.bind")
		for _, g := range pl.Gaps {
			gv := comp.GapView{Kind: g.Kind, Label: "Secret " + g.Key + " (" + g.Env + ")", Field: "secret." + g.Key}
			if fill {
				gv.Post = f.Base + "/gaps/secret?env=" + url.QueryEscape(g.Env)
			}
			box.Review.Plan.Gaps = append(box.Review.Plan.Gaps, gv)
		}
	}
	if i > 0 || (want != "" && i < 0) {
		box.Title = "Plan"
	}
	v.Latest = box
	if i >= 0 || want == "" { // a plan fetched by id is not in the list
		v.Plans = slices.Delete(v.Plans, max(i, 0), max(i, 0)+1)
	}
	return orgui.Config(v), nil
}

func planRow(p service.OrgPlan) comp.PlanRow {
	return comp.PlanRow{
		Status:  p.Status,
		Summary: p.Summary,
		When:    p.CreatedAt,
		Commit:  p.Commit[:min(8, len(p.Commit))],
		Error:   service.ConfigErrLine(p.Error),
	}
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
	// the guided bind: the dialog's body loads when it opens, its lookups are the wizard's
	site.GET(o+"/config/picker", h.orgPicker, bind)
	setup.PickerRoutes(site, h.orch, o+"/config/connector/:connector", bind)
	site.POST(o+"/config/bind", h.orgAction("config", func(c echo.Context, og *service.Org) (string, error) {
		f := c.FormValue
		_, err := h.orch.SetOrgConfigRepo(c.Request().Context(), og.ID, f("connector_id"), strings.TrimSpace(f("repo")), f("branch"), f("path"), false)
		return "Config file saved and planned.", err
	}), bind)
	site.POST(o+"/plan", h.orgAction("config", func(c echo.Context, og *service.Org) (string, error) {
		p, err := h.orch.PlanOrgConfig(c.Request().Context(), og.ID)
		return "Planned: " + p.Summary + ".", err
	}), bind)
	// a secret the pending plan is missing: saved on its env key's scope, then re-planned
	site.POST(o+"/gaps/secret", h.orgAction("config", func(c echo.Context, og *service.Org) (string, error) {
		form, err := c.FormParams()
		if err != nil {
			return "", errs.Invalidf("value", "bad form")
		}
		var es []service.ParamEntry
		for _, e := range render.ParamEntries(form) {
			if e.Kind == "secret" && e.Value != "" {
				es = append(es, e)
			}
		}
		if len(es) == 0 {
			return "", errs.Invalidf("value", "enter the value")
		}
		p, err := h.orch.FillOrgGap(c.Request().Context(), og.ID, c.QueryParam("env"), es)
		return "Saved and planned: " + p.Summary + ".", err
	}), bind)
	site.POST(o+"/plans/:plan/approve", h.orgAction("config", func(c echo.Context, og *service.Org) (string, error) {
		j, err := h.orch.ApproveOrgPlan(c.Request().Context(), c.Param("plan"), render.ApproveOpts(c))
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
	orgLog := func(c echo.Context) error {
		return render.JobLogPage(c, h.orch, "/"+middleware.ScopeOf(c).Org.Slug)
	}
	site.GET("/:org/-/jobs/:job", orgLog, a.Require("deployment.read"))
	site.GET("/:org/-/jobs/:job/log", orgLog, a.Require("deployment.read"))
}

// orgPicker is the bind dialog's body: the org's connectors and every repo
// they can read, as the setup wizard lists them.
func (h *handler) orgPicker(c echo.Context) error {
	og, ctx := middleware.ScopeOf(c).Org, c.Request().Context()
	cs, err := h.orch.ConfigConnectors(ctx, og.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	rs, err := h.orch.OrgConnectorRepos(ctx, og.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	base := "/" + og.Slug + "/-/drawer"
	v := orgui.PickerView{Action: base + "/config/bind", Base: base + "/config/connector"}
	for _, k := range cs {
		label := k.Name + " (" + k.Host + ")"
		if k.OrgID == nil {
			label += ", server"
		}
		v.Connectors = append(v.Connectors, orgui.Option{Value: k.ID, Label: label})
	}
	for _, r := range rs {
		v.Repos = append(v.Repos, comp.RepoOption{FullName: r.FullName, DefaultBranch: r.DefaultBranch, ConnectorID: r.ConnectorID})
	}
	return respond.HTML(c, http.StatusOK, orgui.Picker(v))
}
