package v1

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/orgconf"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Org config-as-code (§6). The runner is assembled from the applier the API
// already carries; the binding itself is edited in the panel (org owner UI).

func (a *API) orgRunner() orgconf.Runner {
	return orgconf.Runner{Store: a.store, Src: a.applier.Planner.Src, Stacks: a.applier.Planner, Applier: a.applier}
}

func (a *API) planOrg(c echo.Context) error {
	ctx := c.Request().Context()
	org, err := a.requireOrg(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := a.requireOrgWrite(ctx, c, org.ID); err != nil {
		return err
	}
	cp, err := a.orgRunner().Plan(ctx, org, "")
	switch err {
	case nil:
	case orgconf.ErrNotBound:
		return echo.NewHTTPError(http.StatusConflict, "org is not bound to a config repo")
	case orgconf.ErrNoFile:
		return echo.NewHTTPError(http.StatusConflict, "no org config file on that branch")
	default:
		return err
	}
	return c.JSON(http.StatusCreated, toPlanOut(cp))
}

// POST /orgs/:id/config/plan-preview, the org twin of the stack preview:
// plan posted file bytes against live state, store nothing. Nothing is written
// and nothing approvable comes back, but running the planner is a write-role
// action like planOrg, so it takes the same org write check. Bound-check
// explicit for the same reason as the stack route: an unbound org has no
// binding context to diff stack refs against.
func (a *API) previewOrgPlan(c echo.Context) error {
	org, err := a.requireOrg(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := a.requireOrgWrite(c.Request().Context(), c, org.ID); err != nil {
		return err
	}
	if !org.ConfigManaged() {
		return echo.NewHTTPError(http.StatusConflict, "org is not bound to a config repo")
	}
	var in orgPreviewIn
	// 2 MB like the stack route: JSON escaping overhead over the raw yaml.
	if err := json.NewDecoder(io.LimitReader(c.Request().Body, 2<<20)).Decode(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request body: "+err.Error())
	}
	if in.Main == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "main is required")
	}
	plan, err := a.orgRunner().PreviewBundle(c.Request().Context(), org, []byte(in.Main))
	switch {
	case err == orgconf.ErrNotBound:
		return echo.NewHTTPError(http.StatusConflict, "org is not bound to a config repo")
	case err != nil:
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "preview failed: "+err.Error())
	}
	blob, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	cp := &repo.ConfigPlan{StackID: org.ID, Status: "preview",
		Summary: plan.Summary(), Plan: string(blob), CreatedAt: time.Now().UTC()}
	return c.JSON(http.StatusOK, toPlanDetail(cp))
}

func (a *API) listOrgPlans(c echo.Context) error {
	org, err := a.requireOrg(c, c.Param("id"))
	if err != nil {
		return err
	}
	plans, err := a.store.ListOrgConfigPlans(c.Request().Context(), org.ID, 20)
	if err != nil {
		return err
	}
	out := make([]planOut, 0, len(plans))
	for i := range plans {
		out = append(out, toPlanOut(&plans[i]))
	}
	return c.JSON(http.StatusOK, out)
}

func (a *API) requireOrgPlan(c echo.Context) (*repo.Org, *repo.ConfigPlan, error) {
	ctx := c.Request().Context()
	cp, err := a.store.GetOrgConfigPlan(ctx, c.Param("id"))
	if err != nil || cp == nil {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "plan not found")
	}
	org, err := a.requireOrg(c, cp.StackID) // org id rides ConfigPlan.StackID
	if err != nil {
		return nil, nil, err
	}
	if err := a.requireOrgWrite(ctx, c, org.ID); err != nil {
		return nil, nil, err
	}
	return org, cp, nil
}

func (a *API) approveOrgPlan(c echo.Context) error {
	org, cp, err := a.requireOrgPlan(c)
	if err != nil {
		return err
	}
	if cp.Status != "pending" {
		return echo.NewHTTPError(http.StatusConflict, "plan is "+cp.Status)
	}
	if err := a.orgRunner().Apply(c.Request().Context(), org, cp); err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
	}
	cp.Status = "applied"
	return c.JSON(http.StatusOK, toPlanOut(cp))
}

func (a *API) rejectOrgPlan(c echo.Context) error {
	_, cp, err := a.requireOrgPlan(c)
	if err != nil {
		return err
	}
	if cp.Status != "pending" {
		return echo.NewHTTPError(http.StatusConflict, "plan is "+cp.Status)
	}
	if err := a.store.SetOrgConfigPlanStatus(c.Request().Context(), cp.ID, "rejected"); err != nil {
		return err
	}
	cp.Status = "rejected"
	return c.JSON(http.StatusOK, toPlanOut(cp))
}
