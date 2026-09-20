package v1

import (
	"net/http"

	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func toDomainOut(d *repo.Domain) domainOut {
	return domainOut{ID: d.ID, Host: d.Host, Path: d.Path, ContainerPort: d.ContainerPort,
		HTTPS: d.HTTPS, ForceHTTPS: d.ForceHTTPS, RedirectTo: d.RedirectTo}
}

func (a *API) listDomains(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	ds, err := a.store.ListDomainsByTile(c.Request().Context(), t.ID)
	if err != nil {
		return err
	}
	out := make([]domainOut, 0, len(ds))
	for i := range ds {
		out = append(out, toDomainOut(&ds[i]))
	}
	return c.JSON(http.StatusOK, out)
}

// createDomain attaches a host to an app and updates the proxy immediately.
// Every rule — squat, wildcard DNS, middleware references, the managed
// engine's HTTP support, the port and HTTPS defaults — is the domain
// service's, and the panel used to apply about three times as many of them
// as this path did.
func (a *API) createDomain(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	var in domainIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	d, staged, err := a.domains.Attach(c.Request().Context(), t, service.DomainSpec{
		Host: in.Host, Path: in.Path, Port: in.ContainerPort,
		HTTPS: in.HTTPS, ForceHTTPS: in.ForceHTTPS, RedirectTo: in.RedirectTo,
		Rule: in.Rule, Priority: in.Priority, Middlewares: in.Middlewares,
	}, a.actor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if staged {
		// A config-managed stack that declares `ui_edits: stage` holds every
		// surface's field edits for review, this one included. Nothing is
		// written, and the API cannot record into the pending set without the
		// canvas's desired-set machinery, so say so rather than answer 201.
		return echo.NewHTTPError(http.StatusConflict,
			"this stack stages edits for review; add the domain from the canvas")
	}
	return c.JSON(http.StatusCreated, toDomainOut(d))
}

// deleteDomain removes a host and re-syncs the proxy.
func (a *API) deleteDomain(c echo.Context) error {
	ctx := c.Request().Context()
	d, err := a.store.GetDomain(ctx, c.Param("id"))
	if err != nil || d == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	t, err := a.tile(c, d.TileID)
	if err != nil {
		return err
	}
	if _, staged, err := a.domains.Detach(ctx, t, d.ID, a.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	} else if staged {
		return echo.NewHTTPError(http.StatusConflict,
			"this stack stages edits for review; remove the domain from the canvas")
	}
	return c.NoContent(http.StatusNoContent)
}
