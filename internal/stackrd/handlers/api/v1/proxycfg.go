package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	yaml "go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Proxy escape hatches over the API (docs/plans/02-serverconfig-migration.md
// §3.2a): the verbatim static override and the named dynamic entries. Same
// storage as /admin/proxy (settings keys) so panel and API stay one truth.

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

func (a *API) proxyEntriesMap(ctx context.Context) map[string]string {
	raw, _ := a.store.GetSetting(ctx, "proxy_custom_dynamic")
	out := map[string]string{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (a *API) saveProxyEntriesMap(ctx context.Context, entries map[string]string) error {
	b, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	if err := a.store.SetSetting(ctx, "proxy_custom_dynamic", string(b)); err != nil {
		return err
	}
	return a.px.SyncCustomDynamic(entries)
}

func yamlValid(s string) error {
	var v map[string]any
	return yaml.Unmarshal([]byte(s), &v)
}

func (a *API) getProxyConfig(c echo.Context) error {
	ctx := c.Request().Context()
	ov, _ := a.store.GetSetting(ctx, "traefik_static_override")
	return c.JSON(http.StatusOK, proxyConfigOut{
		StaticOverride: ov,
		OverrideActive: strings.TrimSpace(ov) != "",
		CurrentStatic:  a.px.CurrentStatic(),
		Entries:        a.proxyEntriesMap(ctx),
	})
}

func (a *API) putProxyOverride(c echo.Context) error {
	ctx := c.Request().Context()
	var in proxyOverrideIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	if strings.TrimSpace(in.YAML) != "" {
		if err := yamlValid(in.YAML); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "not valid YAML: "+err.Error())
		}
	} else {
		in.YAML = ""
	}
	if err := a.store.SetSetting(ctx, "traefik_static_override", in.YAML); err != nil {
		return err
	}
	// Recreate in the background, same as the panel, the API answer should
	// not hang on a container swap.
	go func() { _ = a.px.EnsureTraefik(context.Background()) }()
	return a.getProxyConfig(c)
}

func (a *API) putProxyEntry(c echo.Context) error {
	ctx := c.Request().Context()
	var in proxyEntryIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	name := repo.Slugify(c.Param("name"))
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "entry name required")
	}
	if err := yamlValid(in.YAML); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "not valid YAML: "+err.Error())
	}
	entries := a.proxyEntriesMap(ctx)
	entries[name] = in.YAML
	if err := a.saveProxyEntriesMap(ctx, entries); err != nil {
		return err
	}
	return a.getProxyConfig(c)
}

func (a *API) deleteProxyEntry(c echo.Context) error {
	ctx := c.Request().Context()
	name := c.Param("name")
	entries := a.proxyEntriesMap(ctx)
	if _, ok := entries[name]; !ok {
		return echo.NewHTTPError(http.StatusNotFound, "no such entry")
	}
	delete(entries, name)
	if err := a.saveProxyEntriesMap(ctx, entries); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, struct{}{})
}
