package v1

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/orgconf"
	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Config-as-code over the API: plan, read, approve, reject. This is the surface
// a CI job needs, "what would this push change?" answered before merge, and a
// gated environment applied without anyone opening the canvas.
//
// Approval here is the same act as approval in the panel: it applies with
// force, so deletes go through and the env's manual apply policy is satisfied
// rather than bypassed. That is what "manual" means (someone decides) and the
// decision is bounded by a scope of its own plus a live org write role.

const (
	defaultPlanLimit = 20
	maxPlanLimit     = 100
)

// requireConfigStack loads a stack for a config route. write=true demands a
// live org write role, the API's only other gate is the key's scope, and a
// viewer holding the key would otherwise approve production plans, and also a
// live config binding, since there is nothing to plan or apply without one.
//
// Reads deliberately skip the binding check: unbinding a repo does not erase
// the plans that were applied under it, and the history stays readable.
func (a *API) requireConfigStack(c echo.Context, stackID string, write bool) (*repo.Stack, error) {
	s, err := a.requireStackAccess(c, stackID)
	if err != nil {
		return nil, err
	}
	if !write {
		return s, nil
	}
	if err := a.requireOrgWrite(c.Request().Context(), c, s.OrgID); err != nil {
		return nil, err
	}
	if !s.ConfigManaged() {
		return nil, echo.NewHTTPError(http.StatusConflict, "stack has no config repo bound")
	}
	return s, nil
}

// requirePlan loads a plan and the stack it belongs to, 404ing across tenants.
func (a *API) requirePlan(c echo.Context, write bool) (*repo.Stack, *repo.ConfigPlan, error) {
	cp, err := a.store.GetConfigPlan(c.Request().Context(), c.Param("id"))
	if err != nil || cp == nil {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	s, err := a.requireConfigStack(c, cp.StackID, write)
	if err != nil {
		return nil, nil, err
	}
	return s, cp, nil
}

// GET /stacks/:id/config/plans
func (a *API) listPlans(c echo.Context) error {
	s, err := a.requireConfigStack(c, c.Param("id"), false)
	if err != nil {
		return err
	}
	limit := defaultPlanLimit
	if n, cerr := strconv.Atoi(c.QueryParam("limit")); cerr == nil && n > 0 {
		limit = min(n, maxPlanLimit)
	}
	plans, err := a.store.ListConfigPlans(c.Request().Context(), s.ID, limit)
	if err != nil {
		return err
	}
	out := make([]planOut, 0, len(plans))
	for i := range plans {
		out = append(out, toPlanOut(&plans[i]))
	}
	return c.JSON(http.StatusOK, out)
}

// GET /config/plans/:id
func (a *API) getPlan(c echo.Context) error {
	_, cp, err := a.requirePlan(c, false)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, toPlanDetail(cp))
}

// POST /stacks/:id/config/plan, re-plan now, the same sweep the panel's
// "Plan now" runs (stack branch plus every env pinned to its own).
func (a *API) planStack(c echo.Context) error {
	s, err := a.requireConfigStack(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	plans, err := a.applier.Planner.RunAll(c.Request().Context(), s, "")
	switch {
	case err == stackconf.ErrNoFile:
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "no config file found in the bound repo")
	case err == stackconf.ErrNotBound:
		return echo.NewHTTPError(http.StatusConflict, "stack has no config repo bound")
	case err != nil:
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "plan failed: "+err.Error())
	}
	// Every plan the sweep produced, not just the stack-scoped one: an env
	// pinned to its own branch gets its own plan, and returning one of them
	// would let a pipeline gate on "not destructive" while a staging deletion
	// sits queued next to it.
	out := make([]planDetailOut, 0, len(plans))
	for _, cp := range plans {
		out = append(out, toPlanDetail(cp))
	}
	return c.JSON(http.StatusCreated, out)
}

// POST /stacks/:id/config/plan-preview, plan a posted config bundle against
// live state, terraform-plan style. Stores nothing: no plan row, nothing to
// approve, pending plans untouched. Running the planner is still a write-role
// action like planStack, so it takes the write path, which also brings the
// bound-check: an unbound stack has no branch context to diff against.
func (a *API) previewPlan(c echo.Context) error {
	s, err := a.requireConfigStack(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	var in previewIn
	// inline cap (prhook precedent), no global body middleware.
	// 2 MB, not the CLI's 1 MB raw cap: JSON string escaping (\n, \") can
	// nearly double a yaml payload, and a truncated body reads as a confusing
	// "unexpected EOF" rather than "too big".
	if err := json.NewDecoder(io.LimitReader(c.Request().Body, 2<<20)).Decode(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request body: "+err.Error())
	}
	if in.Main == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "main is required")
	}
	files := make(map[string][]byte, len(in.Files))
	for p, b := range in.Files {
		files[p] = []byte(b)
	}
	plan, err := a.applier.Planner.PreviewBundle(c.Request().Context(), s, []byte(in.Main), files, in.Env)
	switch {
	case err == stackconf.ErrNotBound:
		return echo.NewHTTPError(http.StatusConflict, "stack has no config repo bound")
	case err != nil:
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "preview failed: "+err.Error())
	}
	blob, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	// A synthetic row, never saved, just to reuse the planDetailOut shape.
	// No ID/CommitSHA: there is nothing to approve and no commit involved.
	cp := &repo.ConfigPlan{StackID: s.ID, EnvSlug: in.Env, Status: "preview",
		Summary: plan.Summary(), Plan: string(blob), CreatedAt: time.Now().UTC()}
	return c.JSON(http.StatusOK, toPlanDetail(cp))
}

// POST /config/plans/:id/approve, apply a pending plan.
func (a *API) approvePlan(c echo.Context) error {
	ctx := c.Request().Context()
	s, cp, err := a.requirePlan(c, true)
	if err != nil {
		return err
	}
	if cp.Status != "pending" {
		return echo.NewHTTPError(http.StatusConflict, "plan is not pending (status: "+cp.Status+")")
	}
	// Queued, and the answer says so. An apply builds images and can run for
	// minutes; holding a CI job's HTTP connection open for it is what used to
	// kill it when either end gave up. Poll the plan for the outcome, the row
	// carries the error when it fails.
	if _, err := stackconf.EnqueueApply(ctx, a.work, s, cp, true); err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "could not queue the apply: "+err.Error())
	}
	return c.JSON(http.StatusAccepted, toPlanDetail(cp))
}

// POST /config/plans/:id/reject
func (a *API) rejectPlan(c echo.Context) error {
	ctx := c.Request().Context()
	_, cp, err := a.requirePlan(c, true)
	if err != nil {
		return err
	}
	if cp.Status != "pending" {
		return echo.NewHTTPError(http.StatusConflict, "plan is not pending (status: "+cp.Status+")")
	}
	if err := a.store.SetConfigPlanStatus(ctx, cp.ID, "rejected"); err != nil {
		return err
	}
	cp.Status = "rejected"
	return c.JSON(http.StatusOK, toPlanDetail(cp))
}

// parsePlan decodes a stored plan blob. A row with an unparseable blob is still
// a real plan row, report its status rather than 500ing, the same way the
// panel renders it.
func parsePlan(cp *repo.ConfigPlan) stackconf.Plan {
	var p stackconf.Plan
	if cp.Plan != "" {
		_ = json.Unmarshal([]byte(cp.Plan), &p)
	}
	return p
}

func toPlanOut(cp *repo.ConfigPlan) planOut {
	p := parsePlan(cp)
	out := planOut{
		Destructive: p.Destructive(),
		ID:          cp.ID,
		StackID:     cp.StackID,
		EnvSlug:     cp.EnvSlug,
		CommitSHA:   cp.CommitSHA,
		Status:      cp.Status,
		Summary:     cp.Summary,
		Error:       cp.Error,
		CreatedAt:   cp.CreatedAt.Format(time.RFC3339),
	}
	if cp.DecidedAt.Valid {
		out.DecidedAt = cp.DecidedAt.Time.Format(time.RFC3339)
	}
	return out
}

func toPlanDetail(cp *repo.ConfigPlan) planDetailOut {
	out := planDetailOut{planOut: toPlanOut(cp), Changes: []planChangeOut{}}
	p := parsePlan(cp)
	for _, ch := range p.Changes {
		// Field by field, not a conversion: Change carries render-only detail
		// (Fields) the API does not publish.
		row := planChangeOut{Kind: ch.Kind, Env: ch.Env, Tile: ch.Tile,
			Field: ch.Field, Old: ch.Old, New: ch.New, Note: ch.Note, Destroys: ch.Destroys}
		if ch.Declared() {
			row.Env, row.Scope = "", ch.Env
		}
		out.Changes = append(out.Changes, row)
	}
	out.Errors = p.Errors
	out.Warnings = p.Warnings
	out.GenSecrets = p.GenSecrets
	return out
}

// exportStackConfig renders the stack's live state as the file that would
// produce it.
//
// A panel-first user had no way to move to config-as-code except by
// hand-writing the file and hoping it matched. This is the read side of the
// same serializer the UI-staging path uses, so exporting a stack and planning
// that file against the same stack is an empty diff.
func (a *API) exportStackConfig(c echo.Context) error {
	s, err := a.requireStackAccess(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	state, err := stackconf.Planner{Store: a.store}.Snapshot(ctx, s)
	if err != nil {
		return err
	}
	r := stackconf.StateToResolved(s.Name, state)
	// The ladder order is the store's, which Snapshot does not carry.
	if envs, err := a.store.ListEnvironmentsByStack(ctx, s.ID); err == nil {
		var order []string
		for i := range envs {
			if envs[i].Type == "static" {
				order = append(order, envs[i].Slug)
			}
		}
		if len(order) > 0 {
			r.EnvOrder = order
		}
	}
	out, err := stackconf.ExportYAML(r)
	if err != nil {
		return err
	}
	return c.Blob(http.StatusOK, "application/yaml", out)
}

// exportOrgConfig renders the organization as its config file. Stacks are
// listed as declarations; each one's body lives in its own file.
func (a *API) exportOrgConfig(c echo.Context) error {
	o, err := a.requireOrg(c, c.Param("id"))
	if err != nil {
		return err
	}
	out, err := orgconf.ExportYAML(c.Request().Context(), a.store, o)
	if err != nil {
		return err
	}
	return c.Blob(http.StatusOK, "application/yaml", out)
}
