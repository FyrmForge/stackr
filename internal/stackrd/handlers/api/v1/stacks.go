package v1

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func (a *API) listStacks(c echo.Context) error {
	ps, err := a.store.ListStacks(c.Request().Context())
	if err != nil {
		return err
	}
	out := make([]stackOut, 0, len(ps))
	for _, p := range ps {
		if !a.orgAllowed(c, p.OrgID) {
			continue
		}
		out = append(out, stackOut{ID: p.ID, Name: p.Name, Description: p.Description})
	}
	return c.JSON(http.StatusOK, out)
}

func (a *API) createStack(c echo.Context) error {
	var in stackIn
	if err := c.Bind(&in); err != nil || in.Name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name required")
	}
	orgID, err := a.orgForCreate(c, in.OrgID)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	now := time.Now().UTC()
	p := &repo.Stack{ID: uuid.New().String(), OrgID: orgID, Name: in.Name,
		Slug: repo.Slugify(in.Name), Description: in.Description, Settings: "{}", CreatedAt: now}
	if err := a.store.CreateStack(ctx, p); err != nil {
		return err
	}
	env := &repo.Environment{ID: uuid.New().String(), StackID: p.ID, Name: "Production",
		Slug: "production", Type: "static", Settings: "{}", CreatedAt: now}
	if err := a.store.CreateEnvironment(ctx, env); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, stackOut{ID: p.ID, Name: p.Name, Description: p.Description})
}

func (a *API) deleteStack(c echo.Context) error {
	s, err := a.requireStackAccess(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := a.requireOrgWrite(c.Request().Context(), c, s.OrgID); err != nil {
		return err
	}
	// The rows go on cascade, but the services have to be stopped and the
	// pooled overlays handed back by name first: a freed network that still
	// has services on it is handed to the next claim, possibly another org's.
	// The error is returned rather than swallowed; delete again is the retry.
	if err := a.envOps().TeardownStack(c.Request().Context(), s); err != nil {
		return err
	}
	if err := a.store.DeleteStack(c.Request().Context(), s.ID); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (a *API) listEnvs(c echo.Context) error {
	s, err := a.requireStackAccess(c, c.Param("id"))
	if err != nil {
		return err
	}
	envs, err := a.store.ListEnvironmentsByStack(c.Request().Context(), s.ID)
	if err != nil {
		return err
	}
	out := make([]envOut, 0, len(envs))
	for _, e := range envs {
		out = append(out, envOut{ID: e.ID, Name: e.Name, Slug: e.Slug})
	}
	return c.JSON(http.StatusOK, out)
}

func (a *API) createEnv(c echo.Context) error {
	s, err := a.requireStackAccess(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := a.requireOrgWrite(ctx, c, s.OrgID); err != nil {
		return err
	}
	if err := managedGuard(s); err != nil {
		return err
	}
	var in envIn
	if err := c.Bind(&in); err != nil || in.Name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name required")
	}
	slug := repo.Slugify(in.Name)
	if slug == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name needs a letter or number")
	}
	e := &repo.Environment{ID: uuid.New().String(), StackID: s.ID, Name: in.Name,
		Slug: slug, Type: "static", Settings: "{}", CreatedAt: time.Now().UTC()}
	if err := a.store.CreateEnvironment(ctx, e); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, envOut{ID: e.ID, Name: e.Name, Slug: e.Slug})
}
