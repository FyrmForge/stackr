package org

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/orgconf"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The org-wide plans pages. Config plans are computed asynchronously and then
// wait for someone: before this, an org plan had no URL at all (it rendered
// inline on the settings Config tab) and a stack plan had one nobody could find
// from the org. Both are reachable from here, at the level that owns them.

// planRow is one plan in the list, with the page that reviews it.
type planRow struct {
	Plan repo.ConfigPlan
	Href string
}

// planGroup is one owner's plans: the org itself, or one of its stacks.
type planGroup struct {
	Title string
	Href  string // the owner's own page, for the group heading
	Rows  []planRow
}

// GET /orgs/:id/plans, every plan in this org, the org's own first.
//
// Owner-only, same as the org config tab it grew out of: approving an org plan
// creates and deletes stacks.
func (h *handler) Plans(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := h.ownedSettingsOrg(c)
	if err != nil {
		return err
	}
	var groups []planGroup
	orgPlans, err := h.plans.ForOrg(ctx, o.ID, 20)
	if err != nil {
		return err
	}
	if len(orgPlans) > 0 {
		g := planGroup{Title: o.Name, Href: "/orgs/" + o.Slug + "/settings/config"}
		for _, cp := range orgPlans {
			g.Rows = append(g.Rows, planRow{Plan: cp, Href: "/orgs/" + o.Slug + "/plans/" + cp.ID})
		}
		groups = append(groups, g)
	}
	stacks, err := h.stacks.ListForOrg(ctx, o.ID)
	if err != nil {
		return err
	}
	for _, s := range stacks {
		sp, err := h.plans.ForStack(ctx, s.ID, 10)
		if err != nil {
			return err
		}
		if len(sp) == 0 {
			continue
		}
		g := planGroup{Title: s.Name, Href: "/" + o.Slug + "/" + s.Slug}
		for _, cp := range sp {
			g.Rows = append(g.Rows, planRow{Plan: cp, Href: "/" + o.Slug + "/" + s.Slug + "/plans/" + cp.ID})
		}
		groups = append(groups, g)
	}
	return respond.HTML(c, http.StatusOK, orgPlansPage(c, o, groups))
}

// GET /orgs/:id/plans/:planID, one org plan, the page the banner and the list
// both point at. The stack equivalent lives in handler/project.
func (h *handler) OrgPlanView(c echo.Context) error {
	o, cp, err := h.loadOrgPlan(c)
	if err != nil {
		return err
	}
	work := h.orgPlanWork(c, cp)
	// The apply finished while the banner was polling. Only the banner swaps
	// on a poll, so everything else on screen is still the approve-time
	// render: the plan reads pending and its Approve button is still live,
	// pointing at a plan the next click would 409 on. Bounce the poll into a
	// real navigation and the whole page comes back with the status the plan
	// actually has. Only on the poll, or the navigation it asks for would
	// arrive here and ask for another.
	if work != nil && work.Done() && c.Request().Header.Get("HX-Request") == "true" {
		return respond.Redirect(c, "/orgs/"+o.ID+"/plans/"+cp.ID)
	}
	return respond.HTML(c, http.StatusOK, orgPlanPage(c, o, cp, work))
}

// orgPlanWork is the apply running behind this plan, or nil when none has been
// queued for it. Keyed on the plan id, which is the dedupe key the enqueue
// uses, so an older plan's page can never narrate a newer plan's apply.
func (h *handler) orgPlanWork(c echo.Context, cp *repo.ConfigPlan) *repo.WorkItem {
	w, err := h.store.LatestWorkItem(c.Request().Context(), orgconf.ApplyKind, cp.ID)
	if err != nil {
		return nil
	}
	return w
}

// pendingOrgPlans is what the org canvas banner needs: the newest plan still
// waiting on someone, and how many there are in this org altogether (its own
// plus its stacks'), so the banner can say "one plan" or "go to the list".
func (h *handler) pendingOrgPlans(c echo.Context, o *repo.Org) (*repo.ConfigPlan, int) {
	ctx := c.Request().Context()
	var latest *repo.ConfigPlan
	n := 0
	orgPlans, _ := h.plans.ForOrg(ctx, o.ID, 20)
	for i := range orgPlans {
		if waiting(&orgPlans[i]) {
			if latest == nil {
				latest = &orgPlans[i]
			}
			n++
		}
	}
	stackN, _ := h.plans.AwaitingPlan(ctx, o.ID)
	return latest, n + stackN
}

// waiting: a plan nobody has decided on yet. An errored plan counts, it is
// still the reason the config is not applied.
func waiting(cp *repo.ConfigPlan) bool {
	return cp.Status == "pending" || cp.Status == "error"
}

// planCount is the banner's own copy, kept out of the template.
func planCount(n int) string {
	if n == 1 {
		return "1 plan awaiting review"
	}
	return strconv.Itoa(n) + " plans awaiting review"
}

// planReturn is the ?return= a banner or list row hands the review page, so
// approving lands back where the user started.
func planReturn(self string) string {
	return "?return=" + url.QueryEscape(self)
}
