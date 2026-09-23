package v1

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/orgconf"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Org config-as-code (§6). The runner is assembled from the applier the API
// already carries; the binding itself is edited in the panel (org owner UI).

// orgRunner is the runner wired in main, not a fresh one. An inline
// construction left out the stack service the runner needs to create and bind
// a declared stack the way the panel does.
func (a *API) orgRunner() *orgconf.Runner { return a.orgcfg }

func (a *API) planOrg(c echo.Context) error {
	ctx := c.Request().Context()
	org, err := a.org(c, c.Param("id"))
	if err != nil {
		return err
	}
	// Owner, not content-write. Applying an org plan can rename the org and
	// create or delete stacks, and the panel has always been owner-only here;
	// this path let any member with a config key do it from the CLI. Planning
	// and previewing go with it: a plan is what an approval acts on.
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
	org, err := a.org(c, c.Param("id"))
	if err != nil {
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
	org, err := a.org(c, c.Param("id"))
	if err != nil {
		return err
	}
	plans, err := a.plans.ForOrg(c.Request().Context(), org.ID, 20)
	if err != nil {
		return err
	}
	out := make([]planOut, 0, len(plans))
	for i := range plans {
		out = append(out, toPlanOut(&plans[i]))
	}
	return c.JSON(http.StatusOK, out)
}

// getOrgPlan is one org plan with its full diff. The stack side has had this
// since it existed; the org side had only the list, so `stackr org config
// approve <id>` was an approval of something the operator could not read
// first.
func (a *API) getOrgPlan(c echo.Context) error {
	_, cp, err := a.requireOrgPlan(c)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, toPlanDetail(cp))
}

func (a *API) requireOrgPlan(c echo.Context) (*repo.Org, *repo.ConfigPlan, error) {
	ctx := c.Request().Context()
	cp, err := a.plans.GetOrgPlan(ctx, c.Param("id"))
	if err != nil {
		return nil, nil, stackrmw.HTTP(err)
	}
	org, err := a.org(c, cp.StackID) // org id rides ConfigPlan.StackID
	if err != nil {
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
	// Queued, and the answer says so, the same as the stack side's approve.
	// An org apply probes a bucket per declared share and fetches a repository
	// per declared stack, so holding a CI job's connection open for it is what
	// used to kill it when either end gave up. Poll the plan for the outcome,
	// the row carries the error when it fails.
	if _, err := orgconf.EnqueueApply(c.Request().Context(), a.work, org, cp); err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, err.Error())
	}
	return c.JSON(http.StatusAccepted, toPlanOut(cp))
}

func (a *API) rejectOrgPlan(c echo.Context) error {
	_, cp, err := a.requireOrgPlan(c)
	if err != nil {
		return err
	}
	if cp.Status != "pending" {
		return echo.NewHTTPError(http.StatusConflict, "plan is "+cp.Status)
	}
	if err := a.plans.SetOrgPlanStatus(c.Request().Context(), cp.ID, "rejected"); err != nil {
		return err
	}
	cp.Status = "rejected"
	return c.JSON(http.StatusOK, toPlanOut(cp))
}
