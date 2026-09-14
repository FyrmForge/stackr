package v1

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func toDomainOut(d *repo.Domain) domainOut {
	return domainOut{ID: d.ID, Host: d.Host, Path: d.Path, ContainerPort: d.ContainerPort,
		HTTPS: d.HTTPS, ForceHTTPS: d.ForceHTTPS, RedirectTo: d.RedirectTo}
}

func (a *API) listDomains(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), false)
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
func (a *API) createDomain(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	if err := a.rejectManaged(c.Request().Context(), t.StackID); err != nil {
		return err
	}
	var in domainIn
	if err := c.Bind(&in); err != nil || in.Host == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "host required")
	}
	port := in.ContainerPort
	if port == 0 {
		port = t.ContainerPort
	}
	if port == 0 && in.RedirectTo == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "container port required (set it on the app or the domain)")
	}
	https := true
	if in.HTTPS != nil {
		https = *in.HTTPS
	}
	// Default on: serving TLS used to imply the bounce, so anything that does
	// not say otherwise keeps behaving the way it did.
	force := true
	if in.ForceHTTPS != nil {
		force = *in.ForceHTTPS
	}
	path := in.Path
	if path == "" {
		path = "/"
	}
	ctx := c.Request().Context()
	// One host+path routes to one tile, reject a claim already taken (any org).
	if existing, _ := a.store.GetDomainByHostPath(ctx, in.Host, path); existing != nil {
		return echo.NewHTTPError(http.StatusConflict, "host + path already in use")
	}
	d := &repo.Domain{ID: uuid.New().String(), TileID: t.ID, Host: in.Host, Path: path,
		ContainerPort: port, HTTPS: https, ForceHTTPS: force, RedirectTo: in.RedirectTo,
		CreatedAt: time.Now().UTC()}
	if err := a.store.CreateDomain(ctx, d); err != nil {
		return err
	}
	ds, err := a.store.ListDomainsByTile(ctx, t.ID)
	if err != nil {
		return err
	}
	if err := a.px.WriteApp(t, ds); err != nil {
		return err
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
	t, err := a.requireTile(c, d.TileID, true)
	if err != nil {
		return err
	}
	if err := a.rejectManaged(ctx, t.StackID); err != nil {
		return err
	}
	if err := a.store.DeleteDomain(ctx, d.ID); err != nil {
		return err
	}
	ds, err := a.store.ListDomainsByTile(ctx, t.ID)
	if err != nil {
		return err
	}
	if err := a.px.WriteApp(t, ds); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
