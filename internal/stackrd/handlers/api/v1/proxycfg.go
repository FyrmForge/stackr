package v1

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
)

// Proxy escape hatches over the API (docs/plans/02-serverconfig-migration.md
// §3.2a): the verbatim static override and the named dynamic entries. The
// rules and the storage live in service/proxy, shared with /admin/proxy, so
// panel and API cannot drift.

type proxyConfigOut struct {
	StaticOverride string            `json:"static_override" description:"verbatim traefik.yml replacement; empty = generated default"`
	OverrideActive bool              `json:"override_active"`
	CurrentStatic  string            `json:"current_static" description:"the traefik.yml currently on disk"`
	Entries        map[string]string `json:"entries" description:"custom dynamic entries, name -> raw yaml"`
}

type proxyOverrideIn struct {
	YAML string `json:"yaml" description:"verbatim traefik.yml; empty clears the override"`
}

type proxyEntryIn struct {
	Name string `path:"name"`
	YAML string `json:"yaml" required:"true" description:"raw dynamic-config yaml written as custom-<name>.yml"`
}

type proxyEntryParam struct {
	Name string `path:"name"`
}

func (a *API) getProxyConfig(c echo.Context) error {
	ctx := c.Request().Context()
	ov := a.px.StaticOverride(ctx)
	return c.JSON(http.StatusOK, proxyConfigOut{
		StaticOverride: ov,
		OverrideActive: strings.TrimSpace(ov) != "",
		CurrentStatic:  a.px.CurrentStatic(),
		Entries:        a.px.Entries(ctx),
	})
}

func (a *API) putProxyOverride(c echo.Context) error {
	var in proxyOverrideIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	// Traefik is recreated in the background by the service: the answer should
	// not hang on a container swap, and a client that disconnects must not
	// cancel one half way.
	if err := a.px.SetStaticOverride(c.Request().Context(), in.YAML); err != nil {
		return stackrmw.HTTP(err)
	}
	return a.getProxyConfig(c)
}

func (a *API) putProxyEntry(c echo.Context) error {
	var in proxyEntryIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	if _, err := a.px.SetEntry(c.Request().Context(), c.Param("name"), in.YAML); err != nil {
		return stackrmw.HTTP(err)
	}
	return a.getProxyConfig(c)
}

func (a *API) deleteProxyEntry(c echo.Context) error {
	if err := a.px.DeleteEntry(c.Request().Context(), c.Param("name")); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusOK, struct{}{})
}
