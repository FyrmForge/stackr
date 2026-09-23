package project

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// loadStagingEnv resolves the stack + environment for a staging route and
// checks access.
func (h *handler) loadStagingEnv(c echo.Context) (*repo.Stack, *repo.Environment, error) {
	stack, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return nil, nil, err
	}
	// envURL needs the org slug or it falls back to the org id, which 404s
	// on any install where the two differ (the review breadcrumbs and every
	// post-apply/discard redirect went to /<org-id>/... before this).
	h.fillOrg(c.Request().Context(), stack)
	env, err := h.envs.Get(c.Request().Context(), c.Param("envID"))
	if err != nil || env.StackID != stack.ID {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "environment not found")
	}
	return stack, env, nil
}

// GET /projects/:id/staging/:envID, review the env's pending changes.
func (h *handler) StagingReview(c echo.Context) error {
	ctx := c.Request().Context()
	stack, env, err := h.loadStagingEnv(c)
	if err != nil {
		return err
	}
	changes, err := h.tiles.Staged(ctx, env.ID)
	if err != nil {
		return err
	}
	plan, err := h.applier.StagedPlan(ctx, stack, env.Slug)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, stagingPage(c, stack, env, changes, plan))
}

// POST /projects/:id/staging/:envID/apply, commit the env's staged changes.
func (h *handler) StagingApply(c echo.Context) error {
	ctx := c.Request().Context()
	stack, env, err := h.loadStagingEnv(c)
	if err != nil {
		return err
	}
	// ApplyStaged force-applies structural writes (including deletes). Staged
	// rows can outlive the UI-managed period they were created in, so re-check
	// ownership at apply time, not just at stage time.
	if stack.ConfigManaged() {
		return managedErr(stack)
	}
	ok, err := h.applier.ApplyStaged(ctx, stack, env, true) // human review = approval
	switch {
	case err != nil:
		middleware.SetFlash(c, "Apply failed: "+err.Error(), middleware.FlashError)
	case ok:
		middleware.SetFlash(c, "Changes applied to "+env.Name+".", middleware.FlashSuccess)
	default:
		middleware.SetFlash(c, "Nothing to apply.", middleware.FlashSuccess)
	}
	return respond.Redirect(c, envURL(stack, env))
}

// POST /projects/:id/staging/:envID/discard, drop all of the env's staged changes.
func (h *handler) StagingDiscard(c echo.Context) error {
	ctx := c.Request().Context()
	stack, env, err := h.loadStagingEnv(c)
	if err != nil {
		return err
	}
	if err := h.tiles.DiscardStagedForEnv(ctx, env.ID); err != nil {
		return err
	}
	middleware.SetFlash(c, "Pending changes discarded.", middleware.FlashSuccess)
	return respond.Redirect(c, envURL(stack, env))
}

// POST /projects/:id/staging/:envID/changes/:changeID/discard, drop one.
func (h *handler) StagingDiscardOne(c echo.Context) error {
	ctx := c.Request().Context()
	stack, env, err := h.loadStagingEnv(c)
	if err != nil {
		return err
	}
	sc, err := h.tiles.StagedChange(ctx, c.Param("changeID"))
	if err != nil || sc.EnvID != env.ID {
		return echo.NewHTTPError(http.StatusNotFound, "change not found")
	}
	if err := h.tiles.DiscardStaged(ctx, sc.ID); err != nil {
		return err
	}
	middleware.SetFlash(c, "Change discarded.", middleware.FlashSuccess)
	if n, _ := h.tiles.StagedCount(ctx, env.ID); n == 0 {
		return respond.Redirect(c, envURL(stack, env))
	}
	return respond.Redirect(c, "/projects/"+stack.ID+"/staging/"+env.ID)
}
