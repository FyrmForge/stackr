package org

// The Registry tab. Credentials CI pushes with, and what has been pushed.
//
// Every read is narrowed to the org's own namespace before it is rendered; the
// registry enforces the same boundary on the wire through scoped tokens, so
// this is the second of two locks, not the only one.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/orgconf"
	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// SettingsRegistry renders the tab. A registry that is down leaves the
// credential half working and says so, rather than 500ing the whole page:
// revoking a leaked credential is exactly what you want to still be able to do
// while the registry is unreachable.
// GET /orgs/:slug/settings/registry
func (h *handler) SettingsRegistry(c echo.Context) error {
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	return h.renderRegistry(c, o, "")
}

// renderRegistry draws the tab. newCred is the plaintext of a credential just
// minted, passed straight through rather than stored: the row keeps only a
// hash, so this is the one moment it exists.
func (h *handler) renderRegistry(c echo.Context, o *repo.Org, newCred string) error {
	ctx := c.Request().Context()
	v := registryView{Org: o, CanEdit: h.ownerOf(c, o.ID), NewCred: newCred}
	var err error
	if v.Creds, err = h.registries.Credentials(ctx, o.ID); err != nil {
		return err
	}
	reg, err := h.registries.Managed(ctx)
	if errors.Is(err, svcerr.ErrUnavailable) {
		v.Err = "The managed registry is not configured yet."
		return respond.HTML(c, http.StatusOK, orgRegistryPage(c, v))
	}
	v.Host = reg.PushURL()
	if reg.Domain != "" {
		v.Host = reg.Domain
	}
	v.Open = c.QueryParam("image")
	return respond.HTML(c, http.StatusOK, orgRegistryPage(c, v))
}

// RegistryImages is the Images section, loaded after the page: the catalog and
// the open image's manifests are registry calls, and the credentials above
// must not wait on a registry that is down.
// GET /orgs/:slug/settings/registry/images?image=
func (h *handler) RegistryImages(c echo.Context) error {
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	v := registryView{Org: o, CanEdit: h.ownerOf(c, o.ID)}
	reg, err := h.registries.Managed(ctx)
	if errors.Is(err, svcerr.ErrUnavailable) {
		return echo.NewHTTPError(http.StatusNotFound, "no managed registry")
	}
	cl := registry.NewClient(reg, h.regsign)
	names, err := cl.Images(ctx, o.Slug)
	if err != nil {
		v.Err = "Could not read the registry: " + err.Error()
		return respond.HTML(c, http.StatusOK, registryImages(c, v))
	}
	open := c.QueryParam("image")
	ns := registry.Namespace(o.Slug)
	for _, n := range names {
		im := registryImage{Name: n, Short: strings.TrimPrefix(n, ns), Open: n == open}
		if im.Open {
			// ponytail: one manifest read per tag, in series. Fine for a few
			// dozen tags; parallelise here if an image grows past that.
			im.Tags = h.tagRows(c, o, cl, n)
		}
		v.Images = append(v.Images, im)
	}
	return respond.HTML(c, http.StatusOK, registryImages(c, v))
}

// tagRows expands one image. A manifest that will not read still lists its tag:
// the tag exists, and saying so with no size beats saying nothing.
func (h *handler) tagRows(c echo.Context, o *repo.Org, cl *registry.Client, name string) []registryTag {
	ctx := c.Request().Context()
	tags, err := cl.Tags(ctx, name)
	if err != nil {
		return nil
	}
	live := h.liveTags(ctx, o)
	out := make([]registryTag, 0, len(tags))
	for _, t := range tags {
		row := registryTag{Tag: t, RunsOn: live[name+":"+t]}
		if info, err := cl.Tag(ctx, name, t); err == nil {
			row.Digest, row.SizeMB = info.Digest, info.Size/(1024*1024)
		}
		out = append(out, row)
	}
	return out
}

// liveTags maps "<repository>:<tag>" to the tile that runs it, so the page can
// refuse a delete that a restart would then fail on.
//
// walks the org's stacks and tiles. Tens of rows; add a store query
// if an org ever grows big enough to notice.
func (h *handler) liveTags(ctx context.Context, o *repo.Org) map[string]string {
	out := map[string]string{}
	stacks, err := h.stacks.ListForOrg(ctx, o.ID)
	if err != nil {
		return out
	}
	for _, st := range stacks {
		tiles, err := h.tiles.ListForStack(ctx, st.ID)
		if err != nil {
			continue
		}
		for i := range tiles {
			ds, err := h.deploys.ForTile(ctx, tiles[i].ID, 5)
			if err != nil {
				continue
			}
			for _, d := range ds {
				if d.Status != "done" || d.ImageTag == "" {
					continue
				}
				// The stored tag carries the pull host; the repository path is
				// its tail, which is what identifies the image.
				if _, path, ok := strings.Cut(d.ImageTag, "/"); ok {
					out[path] = st.Slug + "/" + tiles[i].Slug
				}
			}
		}
	}
	return out
}

// CreateRegistryCredential mints one. The secret goes into the session for the
// single render that follows: it is stored hashed, so this is the only moment
// it exists, and a query parameter would put it in the browser history.
// POST /orgs/:slug/settings/registry/credentials
func (h *handler) CreateRegistryCredential(c echo.Context) error {
	o, err := h.ownedSettingsOrg(c)
	if err != nil {
		return err
	}
	// Through the service, which also requires a managed registry to exist:
	// this path minted a credential for a registry that was not there, where
	// the API answers 503.
	_, secret, err := h.registries.MintCredential(c.Request().Context(), o, c.FormValue("name"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	// Rendered here rather than after a redirect: the secret exists only in
	// this request, and a query parameter would put it in browser history.
	return h.renderRegistry(c, o, secret)
}

// DeleteRegistryCredential revokes one.
// POST /orgs/:slug/settings/registry/credentials/delete
func (h *handler) DeleteRegistryCredential(c echo.Context) error {
	o, err := h.ownedSettingsOrg(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := h.registries.RevokeCredential(ctx, o, c.FormValue("id")); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Credential revoked.", middleware.FlashSuccess)
	return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/registry")
}

// DeleteRegistryTag removes one tag. Refused while a live deployment still
// references it: a restart would pull the tag and get a manifest-unknown error
// with nothing left to explain it.
// POST /orgs/:slug/settings/registry/tags/delete
func (h *handler) DeleteRegistryTag(c echo.Context) error {
	o, err := h.ownedSettingsOrg(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	tag := c.FormValue("tag")
	// One name resolver and one in-use matcher, shared with the API. This
	// path checked the prefix only — no repo-name or tag grammar — and its
	// matcher indexed the stored tag by the part after the first slash, so a
	// tag stored without a pull host was deletable while a live deployment
	// still pointed at it.
	name, err := h.registries.Image(ctx, o, c.FormValue("image"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	where, err := h.registries.TagInUse(ctx, o, name, tag)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if where != "" {
		return echo.NewHTTPError(http.StatusConflict, "still deployed on "+where)
	}
	reg, err := h.registries.Managed(ctx)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if err := registry.NewClient(reg, h.regsign).DeleteTag(ctx, name, tag); err != nil {
		middleware.SetFlash(c, "Could not delete it: "+err.Error(), middleware.FlashError)
		return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/registry?image="+name)
	}
	middleware.SetFlash(c, "Deleted. The space comes back at the next nightly cleanup.", middleware.FlashSuccess)
	return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/registry?image="+name)
}

// SettingsDefaults renders the org's rung of the defaults cascade.
// GET /orgs/:slug/settings/defaults
func (h *handler) SettingsDefaults(c echo.Context) error {
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	return respond.HTML(c, http.StatusOK, orgDefaultsPage(c, o,
		settings.Parse(o.Settings), settings.ForServer(ctx, h.store), h.ownerOf(c, o.ID)))
}

// SaveDefaults writes them. A field left empty clears the override, which is
// how the org gives a knob back to the server.
// POST /orgs/:slug/settings/defaults
func (h *handler) SaveDefaults(c echo.Context) error {
	o, err := h.ownedSettingsOrg(c)
	if err != nil {
		return err
	}
	vals, err := c.FormParams()
	if err != nil {
		return err
	}
	// Flash, not a 400: the form posts over htmx, which renders an error body
	// nowhere, so a bare status is a save that silently does nothing.
	if err := h.settings.SaveOrg(c.Request().Context(), o, vals); err != nil {
		if !stackrmw.FlashRefusal(c, err) {
			return err
		}
		return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/defaults")
	}
	middleware.SetFlash(c, "Defaults saved.", middleware.FlashSuccess)
	return respond.Redirect(c, "/orgs/"+o.Slug+"/settings/defaults")
}

// ExportConfig downloads the org file. GET /orgs/:slug/settings/config/export
func (h *handler) ExportConfig(c echo.Context) error {
	o, err := h.settingsOrg(c)
	if err != nil {
		return err
	}
	out, err := orgconf.ExportYAML(c.Request().Context(), h.store, o)
	if err != nil {
		return err
	}
	c.Response().Header().Set("Content-Disposition", `attachment; filename="stackr-org.yml"`)
	return c.Blob(http.StatusOK, "application/yaml", out)
}
