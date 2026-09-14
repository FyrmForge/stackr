package project

import (
	"context"
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envcompare"
	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The environments pill and panel on the stack canvas: how every environment
// differs from the first one. Nothing here knows about commits or plans.

// compareStack builds the comparison for one stack: static envs in ladder
// order, the planner's snapshot, and the intended marks.
func (h *handler) compareStack(ctx context.Context, p *repo.Stack) (envcompare.Result, map[string]string, error) {
	envs, err := h.store.ListEnvironmentsByStack(ctx, p.ID)
	if err != nil {
		return envcompare.Result{}, nil, err
	}
	colors := h.envColorsByID(ctx, p, envs)
	var statics []repo.Environment
	for _, e := range envs {
		if e.Type != "ephemeral" {
			statics = append(statics, e)
		}
	}
	state, err := stackconf.Planner{Store: h.store}.Snapshot(ctx, p)
	if err != nil {
		return envcompare.Result{}, nil, err
	}
	intended := map[string][]repo.Intended{}
	for _, e := range statics {
		rows, _ := h.store.ListIntended(ctx, e.ID)
		intended[e.ID] = rows
	}
	return envcompare.Compare(p.Name, statics, state, intended), colors, nil
}

// EnvCompare re-renders the widget: on open, on every project event.
// GET /projects/:id/envs/compare
func (h *handler) EnvCompare(c echo.Context) error {
	p, err := h.loadStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	return h.renderCompare(c, p)
}

func (h *handler) renderCompare(c echo.Context, p *repo.Stack) error {
	ctx := c.Request().Context()
	res, colors, err := h.compareStack(ctx, p)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, envCompareWidget(c, p, res, colors, h.commitLog(ctx, p)))
}

// compareEnv loads the env behind a panel action and the cell it acts on.
func (h *handler) compareEnv(c echo.Context) (*repo.Stack, *repo.Environment, envcompare.Result, envcompare.Cell, error) {
	ctx := c.Request().Context()
	env, err := h.store.GetEnvironment(ctx, c.Param("id"))
	if err != nil {
		return nil, nil, envcompare.Result{}, envcompare.Cell{}, err
	}
	if env == nil {
		return nil, nil, envcompare.Result{}, envcompare.Cell{}, echo.NewHTTPError(http.StatusNotFound, "environment not found")
	}
	p, err := h.loadStack(c, env.StackID)
	if err != nil {
		return nil, nil, envcompare.Result{}, envcompare.Cell{}, err
	}
	res, _, err := h.compareStack(ctx, p)
	if err != nil {
		return nil, nil, envcompare.Result{}, envcompare.Cell{}, err
	}
	slug := c.FormValue("tile")
	for _, row := range res.Rows {
		if row.Slug != slug {
			continue
		}
		for _, cell := range row.Cells {
			if cell.EnvID == env.ID {
				return p, env, res, cell, nil
			}
		}
	}
	return nil, nil, envcompare.Result{}, envcompare.Cell{}, echo.NewHTTPError(http.StatusNotFound, "tile not found in this environment")
}

// MarkIntended flags every differing key of one tile in one env as on
// purpose, at its value now. A later change to a key shows up again.
// POST /envs/:id/intended
func (h *handler) MarkIntended(c echo.Context) error {
	ctx := c.Request().Context()
	p, env, _, cell, err := h.compareEnv(c)
	if err != nil {
		return err
	}
	for _, k := range cell.Keys {
		if k.Intended {
			continue
		}
		if err := h.store.SetIntended(ctx, &repo.Intended{EnvironmentID: env.ID, TileSlug: c.FormValue("tile"), Key: k.Name, Value: k.Val}); err != nil {
			return err
		}
	}
	return h.renderCompare(c, p)
}

// CopyEnv writes the reference env's values for one tile into this env and
// deploys it. The file owns tiles on a managed stack, so that is refused.
// POST /envs/:id/copy
func (h *handler) CopyEnv(c echo.Context) error {
	ctx := c.Request().Context()
	p, env, res, cell, err := h.compareEnv(c)
	if err != nil {
		return err
	}
	if p.ConfigManaged() {
		return managedErr(p)
	}
	if cell.State == "missing" {
		return echo.NewHTTPError(http.StatusBadRequest, "this environment does not have that tile")
	}
	state, err := stackconf.Planner{Store: h.store}.Snapshot(ctx, p)
	if err != nil {
		return err
	}
	r := stackconf.StateToResolved(p.Name, state)
	slug := c.FormValue("tile")
	tc, ok := r.Envs[res.Envs[0].Slug].Tiles[slug]
	if !ok {
		return echo.NewHTTPError(http.StatusBadRequest, "the reference environment does not have that tile")
	}
	// Hostnames are the one thing that is per environment by construction.
	tc.Domains = r.Envs[env.Slug].Tiles[slug].Domains
	if err := h.applier.CopyTile(ctx, p, env, slug, tc, cell.Fields()); err != nil {
		return err
	}
	if h.jobs != nil {
		_ = h.jobs.LoadSchedules(ctx)
	}
	return h.renderCompare(c, p)
}
