package v1

// Environment lifecycle: delete, reset, copy, and the two settings the panel
// could change and nothing else could.
//
// Deleting or resetting an environment takes its containers with it, so both
// need `force: true` once anything is running there. That is the API's version
// of the panel's type-the-slug confirmation: a script has no dialogue to show,
// and a flag it had to add on purpose is the same deliberate act.

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/envcolor"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// requireEnvWrite resolves the env and checks write access to its org.
func (a *API) requireEnvWrite(c echo.Context, envID string) (*repo.Environment, *repo.Stack, error) {
	env, err := a.requireEnvAccess(c, envID)
	if err != nil {
		return nil, nil, err
	}
	s, err := a.store.GetStack(c.Request().Context(), env.StackID)
	if err != nil || s == nil {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err := a.requireOrgWrite(c.Request().Context(), c, s.OrgID); err != nil {
		return nil, nil, err
	}
	return env, s, nil
}

// envRunning reports whether anything is deployed in the environment, which is
// what makes a delete or reset destructive rather than tidy-up.
func (a *API) envRunning(c echo.Context, envID string) bool {
	tiles, err := a.store.ListTilesByEnv(c.Request().Context(), envID)
	if err != nil {
		return false
	}
	for i := range tiles {
		switch tiles[i].Status {
		case "", "idle", "stopped":
		default:
			return true
		}
	}
	return false
}

func (a *API) deleteEnv(c echo.Context) error {
	env, s, err := a.requireEnvWrite(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := a.rejectManaged(ctx, s.ID); err != nil {
		return err
	}
	envs, err := a.store.ListEnvironmentsByStack(ctx, s.ID)
	if err != nil {
		return err
	}
	if len(envs) <= 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "a stack needs at least one environment")
	}
	var in forceIn
	_ = c.Bind(&in)
	if !in.Force && a.envRunning(c, env.ID) {
		return echo.NewHTTPError(http.StatusConflict,
			"tiles are running in "+env.Slug+"; pass force: true to tear them down")
	}
	if err := a.envOps().Teardown(ctx, s, env); err != nil {
		return err
	}
	if a.jobs != nil {
		_ = a.jobs.LoadSchedules(ctx)
	}
	return c.NoContent(http.StatusNoContent)
}

// resetEnv tears a config-managed stack's environment down so the next apply
// rebuilds it from the file. A half-applied env had no way back otherwise.
func (a *API) resetEnv(c echo.Context) error {
	env, s, err := a.requireEnvWrite(c, c.Param("id"))
	if err != nil {
		return err
	}
	if !s.ConfigManaged() {
		return echo.NewHTTPError(http.StatusBadRequest,
			"reset is for config-managed stacks; delete the environment instead")
	}
	var in forceIn
	_ = c.Bind(&in)
	if !in.Force && a.envRunning(c, env.ID) {
		return echo.NewHTTPError(http.StatusConflict,
			"tiles are running in "+env.Slug+"; pass force: true to tear them down")
	}
	ctx := c.Request().Context()
	if err := a.envOps().Teardown(ctx, s, env); err != nil {
		return err
	}
	if a.jobs != nil {
		_ = a.jobs.LoadSchedules(ctx)
	}
	return c.NoContent(http.StatusNoContent)
}

// copyEnv creates a new environment from an existing one: same tiles, nothing
// deployed. The slug is derived from the name, like every other slug.
func (a *API) copyEnv(c echo.Context) error {
	src, s, err := a.requireEnvWrite(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := a.rejectManaged(ctx, s.ID); err != nil {
		return err
	}
	var in copyEnvIn
	if err := c.Bind(&in); err != nil || strings.TrimSpace(in.Name) == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name required")
	}
	slug := repo.Slugify(in.Name)
	if slug == "" || slug == "settings" || slug == "list" || slug == repo.HomeSlug {
		return echo.NewHTTPError(http.StatusBadRequest, "that name is reserved or has no slug in it")
	}
	if existing, _ := a.store.GetEnvironmentBySlug(ctx, s.ID, slug); existing != nil {
		return echo.NewHTTPError(http.StatusConflict, "an environment with that name already exists")
	}
	envType := "static"
	if in.Ephemeral {
		envType = "ephemeral"
	}
	env := &repo.Environment{ID: uuid.New().String(), StackID: s.ID, Name: strings.TrimSpace(in.Name),
		Slug: slug, Type: envType, BaseEnvID: src.ID, Settings: src.Settings, CreatedAt: time.Now().UTC()}
	if err := a.store.CreateEnvironment(ctx, env); err != nil {
		return err
	}
	if err := a.envOps().CloneTiles(ctx, env); err != nil {
		return err
	}
	if a.jobs != nil {
		_ = a.jobs.LoadSchedules(ctx)
	}
	return c.JSON(http.StatusCreated, envOut{ID: env.ID, Name: env.Name, Slug: env.Slug})
}

// patchEnv changes the two per-environment settings that were panel-only: the
// colour the canvas draws it in, and whether a plan touching it applies on its
// own or waits for a person.
//
// `protected` is deliberately absent: it is a config-file key that gates
// deletes during an apply, not a column, so there is nothing here to set.
func (a *API) patchEnv(c echo.Context) error {
	env, _, err := a.requireEnvWrite(c, c.Param("id"))
	if err != nil {
		return err
	}
	var in envPatch
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if in.Color != nil {
		if !envcolor.Valid(*in.Color) {
			return echo.NewHTTPError(http.StatusBadRequest, "color must be a palette name or #rrggbb")
		}
		env.Color = *in.Color
	}
	if in.ApplyPolicy != nil {
		switch *in.ApplyPolicy {
		case "", "auto", "manual":
			env.ApplyPolicy = *in.ApplyPolicy
		default:
			return echo.NewHTTPError(http.StatusBadRequest, "apply_policy must be auto, manual or empty")
		}
	}
	if err := a.store.UpdateEnvironment(c.Request().Context(), env); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, envOut{ID: env.ID, Name: env.Name, Slug: env.Slug})
}

// envOps is the shared environment machinery: teardown, cloning, auto domains.
// Built on demand, like the panel's.
func (a *API) envOps() envops.Ops {
	return envops.Ops{Store: a.store, RT: a.clus.Runtime(), Cluster: a.clus, PX: a.px,
		DBs: managedtiles.NewService(a.clus, a.store)}
}
