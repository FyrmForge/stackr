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

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// envAndStack loads an environment and the stack it belongs to. It used to
// check write access as well; the route's gate does that now.
func (a *API) envAndStack(c echo.Context, envID string) (*repo.Environment, *repo.Stack, error) {
	env, err := a.env(c, envID)
	if err != nil {
		return nil, nil, err
	}
	s, err := a.stacks.Get(c.Request().Context(), env.StackID)
	if err != nil {
		return nil, nil, stackrmw.HTTP(err)
	}
	return env, s, nil
}

// envRunning reports whether anything is deployed in the environment, which is
// what makes a delete or reset destructive rather than tidy-up.
func (a *API) envRunning(c echo.Context, envID string) bool {
	tiles, err := a.tiles.ListForEnv(c.Request().Context(), envID)
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
	env, s, err := a.envAndStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	var in forceIn
	_ = c.Bind(&in)
	if err := a.envs.Delete(c.Request().Context(), s, env, in.Force, a.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// resetEnv tears a config-managed stack's environment down so the next apply
// rebuilds it from the file. A half-applied env had no way back otherwise.
func (a *API) resetEnv(c echo.Context) error {
	env, s, err := a.envAndStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	var in forceIn
	_ = c.Bind(&in)
	// The replan is the service's, and this path never had one: a reset over
	// the API left the stack's plans describing tiles that no longer existed.
	if err := a.envs.Reset(c.Request().Context(), s, env, in.Force, a.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// copyEnv creates a new environment from an existing one: same tiles, nothing
// deployed. The slug is derived from the name, like every other slug.
func (a *API) copyEnv(c echo.Context) error {
	src, s, err := a.envAndStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	var in copyEnvIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	envType := ""
	if in.Ephemeral {
		envType = "ephemeral"
	}
	env, err := a.envs.Create(c.Request().Context(), s, service.CreateEnv{
		Name: in.Name, Type: envType, BaseEnvID: src.ID,
	}, a.actor(c))
	if err != nil {
		return stackrmw.HTTP(err)
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
	env, _, err := a.envAndStack(c, c.Param("id"))
	if err != nil {
		return err
	}
	var in envPatch
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if err := a.envs.Update(c.Request().Context(), env, service.EnvPatch{
		Color: in.Color, ApplyPolicy: in.ApplyPolicy,
	}, a.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusOK, envOut{ID: env.ID, Name: env.Name, Slug: env.Slug})
}

// envOps is the shared environment machinery: teardown, cloning, auto domains.
// Built on demand, like the panel's.
func (a *API) envOps() envops.Ops {
	return envops.Ops{Store: a.store, RT: a.clus.Runtime(), Cluster: a.clus, PX: a.px,
		DBs: managedtiles.NewService(a.clus, a.store), Tiles: a.tiles, Sched: a.sched, Domains: a.domains, Resources: a.resources}
}
