package v1

import (
	"net/http"

	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
)

func (a *API) listStacks(c echo.Context) error {
	ps, err := a.stacks.ListAll(c.Request().Context())
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
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	orgID, err := a.orgForCreate(c, in.OrgID)
	if err != nil {
		return err
	}
	// The empty name, the slug with nothing in it and the duplicate are all
	// the service's to refuse. This path checked none of them, so a second
	// stack with the same name hit UNIQUE (org_id, slug) and came back a 500.
	p, err := a.stacks.Create(c.Request().Context(), orgID,
		service.CreateStack{Name: in.Name, Description: in.Description})
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusCreated, stackOut{ID: p.ID, Name: p.Name, Description: p.Description})
}

func (a *API) deleteStack(c echo.Context) error {
	s, err := a.stack(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := a.stacks.Delete(c.Request().Context(), s); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (a *API) listEnvs(c echo.Context) error {
	s, err := a.stack(c, c.Param("id"))
	if err != nil {
		return err
	}
	envs, err := a.envs.ListForStack(c.Request().Context(), s.ID)
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
	s, err := a.stack(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var in envIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	// This path checked only that the name was non-empty: no reserved names,
	// no duplicate check. An environment named "Stack" collided with the
	// hidden home row and surfaced as a raw 500.
	e, err := a.envs.Create(ctx, s, service.CreateEnv{Name: in.Name}, a.actor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusCreated, envOut{ID: e.ID, Name: e.Name, Slug: e.Slug})
}
