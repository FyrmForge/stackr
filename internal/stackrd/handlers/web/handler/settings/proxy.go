package settings

import (
	"net/http"
	"sort"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
)

// The proxy escape hatches (docs/plans/02-serverconfig-migration.md 3.2a):
// operator-named dynamic entries written into the traefik dynamic dir, and a
// verbatim static-config override. The rules live in service/proxy, which the
// API's /proxy routes share; these handlers only bind and render.

// GET /admin/proxy
func (h *handler) ProxyPage(c echo.Context) error {
	ctx := c.Request().Context()
	entries := h.px.Entries(ctx)
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	typed, trustCF := h.px.TrustedProxies(ctx)
	return respond.HTML(c, http.StatusOK,
		proxyPage(c, h.px.CurrentStatic(), h.px.StaticOverride(ctx), names, entries, typed, trustCF))
}

// POST /admin/proxy/trusted, the CIDRs Traefik takes X-Forwarded-For from.
// Traefik is recreated in the background.
func (h *handler) SaveTrustedProxies(c echo.Context) error {
	err := h.px.SetTrustedProxies(c.Request().Context(),
		c.FormValue("trusted_proxies"), c.FormValue("trust_cloudflare") == "1")
	if err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Trusted proxies saved.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/proxy")
}

// POST /admin/proxy/override, verbatim static-config override; empty reverts
// to the generated default. Traefik is recreated in the background.
func (h *handler) SaveProxyOverride(c echo.Context) error {
	ov := c.FormValue("static_override")
	if err := h.px.SetStaticOverride(c.Request().Context(), ov); err != nil {
		return stackrmw.HTTP(err)
	}
	msg := "Static override saved. Traefik restarts with it."
	if h.px.StaticOverride(c.Request().Context()) == "" {
		msg = "Override cleared. Traefik restarts on the generated config."
	}
	middleware.SetFlash(c, msg, middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/proxy")
}

// POST /admin/proxy/entry, create/update one named dynamic entry.
func (h *handler) SaveProxyEntry(c echo.Context) error {
	name, err := h.px.SetEntry(c.Request().Context(), c.FormValue("name"), c.FormValue("yaml"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Entry "+name+" written to the dynamic dir. Traefik picks it up live.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/proxy")
}

// POST /admin/proxy/entry/delete
func (h *handler) DeleteProxyEntry(c echo.Context) error {
	name := c.FormValue("name")
	if err := h.px.DeleteEntry(c.Request().Context(), name); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Entry "+name+" removed.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/proxy")
}
