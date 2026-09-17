package settings

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"
	yaml "go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/netaddr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The proxy escape hatches (docs/plans/02-serverconfig-migration.md §3.2a):
// operator-named dynamic entries written into the traefik dynamic dir, and a
// verbatim static-config override. Both live in the settings KV table so a
// wiped data dir heals from the DB on the next Resync/EnsureTraefik.

func (h *handler) proxyEntries(ctx context.Context) map[string]string {
	raw, _ := h.store.GetSetting(ctx, "proxy_custom_dynamic")
	out := map[string]string{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (h *handler) saveProxyEntries(ctx context.Context, entries map[string]string) error {
	b, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	if err := h.store.SetSetting(ctx, "proxy_custom_dynamic", string(b)); err != nil {
		return err
	}
	return h.px.SyncCustomDynamic(entries)
}

func validYAML(s string) error {
	var v map[string]any
	return yaml.Unmarshal([]byte(s), &v)
}

// GET /admin/proxy
func (h *handler) ProxyPage(c echo.Context) error {
	ctx := c.Request().Context()
	override, _ := h.store.GetSetting(ctx, "traefik_static_override")
	entries := h.proxyEntries(ctx)
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	typed, _ := h.store.GetSetting(ctx, "trusted_proxies")
	trustCF, _ := h.store.GetSetting(ctx, "trust_cloudflare")
	return respond.HTML(c, http.StatusOK, proxyPage(c, h.px.CurrentStatic(), override, names, entries, typed, trustCF == "1"))
}

// POST /admin/proxy/trusted, the CIDRs Traefik takes X-Forwarded-For from.
// Traefik is recreated in the background.
func (h *handler) SaveTrustedProxies(c echo.Context) error {
	ctx := c.Request().Context()
	var lines []string
	for _, line := range strings.Split(c.FormValue("trusted_proxies"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cidr, err := netaddr.ParseTrusted(line)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		lines = append(lines, cidr)
	}
	trustCF := ""
	if c.FormValue("trust_cloudflare") == "1" {
		trustCF = "1"
		if err := h.px.RefreshCloudflare(ctx); err != nil {
			if cached, _ := h.store.GetSetting(ctx, "cloudflare_cidrs"); strings.TrimSpace(cached) == "" {
				slog.Warn("cloudflare ranges fetch failed", "error", err)
				return echo.NewHTTPError(http.StatusBadGateway, "could not reach Cloudflare")
			}
			slog.Warn("cloudflare ranges fetch failed, using cache", "error", err)
		}
	}
	if err := h.store.SetSetting(ctx, "trusted_proxies", strings.Join(lines, "\n")); err != nil {
		return err
	}
	if err := h.store.SetSetting(ctx, "trust_cloudflare", trustCF); err != nil {
		return err
	}
	go func() {
		if err := h.px.EnsureTraefik(context.Background()); err != nil {
			slog.Error("traefik restart after trusted proxies change failed", "error", err)
		}
	}()
	middleware.SetFlash(c, "Trusted proxies saved.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/proxy")
}

// POST /admin/proxy/override, verbatim static-config override; empty reverts
// to the generated default. Traefik is recreated in the background.
func (h *handler) SaveProxyOverride(c echo.Context) error {
	ctx := c.Request().Context()
	ov := c.FormValue("static_override")
	if strings.TrimSpace(ov) != "" {
		if err := validYAML(ov); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "not valid YAML: "+err.Error())
		}
	} else {
		ov = ""
	}
	if err := h.store.SetSetting(ctx, "traefik_static_override", ov); err != nil {
		return err
	}
	go func() {
		if err := h.px.EnsureTraefik(context.Background()); err != nil {
			slog.Error("traefik restart after static override failed", "error", err)
		}
	}()
	msg := "Static override saved. Traefik restarts with it."
	if ov == "" {
		msg = "Override cleared. Traefik restarts on the generated config."
	}
	middleware.SetFlash(c, msg, middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/proxy")
}

// POST /admin/proxy/entry, create/update one named dynamic entry.
func (h *handler) SaveProxyEntry(c echo.Context) error {
	ctx := c.Request().Context()
	name := repo.Slugify(c.FormValue("name"))
	body := c.FormValue("yaml")
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "entry name required")
	}
	if err := validYAML(body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "not valid YAML: "+err.Error())
	}
	entries := h.proxyEntries(ctx)
	entries[name] = body
	if err := h.saveProxyEntries(ctx, entries); err != nil {
		return err
	}
	middleware.SetFlash(c, "Entry "+name+" written to the dynamic dir. Traefik picks it up live.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/proxy")
}

// POST /admin/proxy/entry/delete
func (h *handler) DeleteProxyEntry(c echo.Context) error {
	ctx := c.Request().Context()
	name := c.FormValue("name")
	entries := h.proxyEntries(ctx)
	if _, ok := entries[name]; !ok {
		return echo.NewHTTPError(http.StatusNotFound, "no such entry")
	}
	delete(entries, name)
	if err := h.saveProxyEntries(ctx, entries); err != nil {
		return err
	}
	middleware.SetFlash(c, "Entry "+name+" removed.", middleware.FlashSuccess)
	return respond.Redirect(c, "/admin/proxy")
}
