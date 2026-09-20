package v1

// The org registry surface: push credentials, and the images they produced.
//
// Every read here is filtered to the caller's org before it is rendered. The
// registry itself enforces the same boundary on the wire (tokens are scoped to
// one namespace), so this is the second of two locks, not the only one.

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// requireOrgRegistry resolves the org and the managed registry together: every
// route here needs both, and a missing registry is a 503 rather than a 404,
// because the org is fine and the thing that is down is ours.
func (a *API) requireOrgRegistry(c echo.Context, write bool) (*repo.Org, *repo.Registry, error) {
	o, err := a.org(c, c.Param("id"))
	if err != nil {
		return nil, nil, err
	}
	reg, err := a.store.GetManagedRegistry(c.Request().Context())
	if err != nil {
		return nil, nil, err
	}
	if reg == nil {
		return nil, nil, echo.NewHTTPError(http.StatusServiceUnavailable, "the managed registry is not configured")
	}
	return o, reg, nil
}

func (a *API) listRegistryCredentials(c echo.Context) error {
	o, err := a.org(c, c.Param("id"))
	if err != nil {
		return err
	}
	creds, err := a.store.ListOrgRegistryCredentials(c.Request().Context(), o.ID)
	if err != nil {
		return err
	}
	out := make([]registryCredentialOut, 0, len(creds))
	for i := range creds {
		out = append(out, toRegistryCredentialOut(&creds[i], ""))
	}
	return c.JSON(http.StatusOK, out)
}

// createRegistryCredential mints one. The secret comes back exactly once: it is
// stored hashed, so there is no second chance to read it, and saying so in the
// response shape is better than a caller discovering it on the next GET.
func (a *API) createRegistryCredential(c echo.Context) error {
	o, _, err := a.requireOrgRegistry(c, true)
	if err != nil {
		return err
	}
	var in registryCredentialIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	cred, secret, err := a.registries.MintCredential(c.Request().Context(), o, in.Name)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusCreated, toRegistryCredentialOut(cred, secret))
}

func (a *API) deleteRegistryCredential(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := a.org(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := a.registries.RevokeCredential(ctx, o, c.Param("cred")); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func toRegistryCredentialOut(c *repo.OrgRegistryCredential, secret string) registryCredentialOut {
	out := registryCredentialOut{ID: c.ID, Name: c.Name, Prefix: c.Prefix,
		System: c.System, CreatedAt: c.CreatedAt, Secret: secret}
	if c.LastUsedAt.Valid {
		out.LastUsedAt = &c.LastUsedAt.Time
	}
	return out
}

func (a *API) listRegistryImages(c echo.Context) error {
	o, reg, err := a.requireOrgRegistry(c, false)
	if err != nil {
		return err
	}
	names, err := registry.NewClient(reg, a.signer).Images(c.Request().Context(), o.Slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	out := make([]registryImageOut, 0, len(names))
	for _, n := range names {
		out = append(out, registryImageOut{Name: n, Short: strings.TrimPrefix(n, registry.Namespace(o.Slug))})
	}
	return c.JSON(http.StatusOK, out)
}

// orgImage resolves the repository named in the path and refuses one outside
// the caller's namespace.
//
// echo 4 routes on the raw path and does NOT unescape Param, so the decode is
// this function's job: without it %2e%2e%2f arrives as a literal ".." further
// down. Syntax is the registry client's rail (repoName in infra/registry/gc.go,
// which admits no slash at all); what is left here is authorization, the
// namespace prefix.
// orgImage resolves the :name path segment inside the org's namespace. The
// resolution itself is the registry service's, shared with the panel; what
// stays here is unescaping the path and the 404 wording (the registry
// client's own error comes back as a 502 repeating the namespaced name,
// which reads as "the registry is broken" for a bad path from the caller).
func (a *API) orgImage(c echo.Context, o *repo.Org) (string, error) {
	raw, err := url.PathUnescape(c.Param("name"))
	if err != nil {
		return "", echo.NewHTTPError(http.StatusBadRequest, "malformed image name")
	}
	name, err := a.registries.Image(c.Request().Context(), o, raw)
	if err != nil {
		return "", echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	return name, nil
}

func (a *API) listRegistryTags(c echo.Context) error {
	o, reg, err := a.requireOrgRegistry(c, false)
	if err != nil {
		return err
	}
	name, err := a.orgImage(c, o)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	cl := registry.NewClient(reg, a.signer)
	tags, err := cl.Tags(ctx, name)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	out := make([]registryTagOut, 0, len(tags))
	for _, t := range tags {
		info, err := cl.Tag(ctx, name, t)
		if err != nil {
			// One unreadable manifest must not blank the whole list: the tag
			// exists, and saying so with no size beats saying nothing.
			out = append(out, registryTagOut{Tag: t})
			continue
		}
		out = append(out, registryTagOut{Tag: info.Tag, Digest: info.Digest, Size: info.Size})
	}
	return c.JSON(http.StatusOK, out)
}

// deleteRegistryTag removes a tag, unless something running still references
// it. The blobs stay until the nightly garbage collection.
func (a *API) deleteRegistryTag(c echo.Context) error {
	o, reg, err := a.requireOrgRegistry(c, true)
	if err != nil {
		return err
	}
	name, err := a.orgImage(c, o)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	tag, err := url.PathUnescape(c.Param("tag"))
	if err != nil || !registry.ValidTag(tag) {
		return echo.NewHTTPError(http.StatusBadRequest, "malformed tag")
	}
	// Same rule as pruning: an image a live deployment points at is what a
	// restart would pull, so deleting it turns the next restart into a
	// manifest-unknown failure with nothing left to explain it.
	where, err := a.registries.TagInUse(ctx, o, name, tag)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if where != "" {
		return echo.NewHTTPError(http.StatusConflict, "still deployed on "+where)
	}
	if err := registry.NewClient(reg, a.signer).DeleteTag(ctx, name, tag); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// --- admin: the registries themselves ---
//
// The managed registry's TLS domain, and the external registries a tile can
// pull from. Server-wide, so admin-only: an external registry row carries
// credentials, and the managed one's domain decides how every node pulls.

func (a *API) listRegistries(c echo.Context) error {
	regs, err := a.store.ListRegistries(c.Request().Context())
	if err != nil {
		return err
	}
	out := make([]registryOut, 0, len(regs))
	for i := range regs {
		out = append(out, toRegistryOut(&regs[i]))
	}
	return c.JSON(http.StatusOK, out)
}

// createRegistry records an external registry. The password is write-only: it
// goes in here and never comes back out of a GET.
func (a *API) createRegistry(c echo.Context) error {
	var in registryIn
	if err := c.Bind(&in); err != nil || strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.URL) == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name and url required")
	}
	r := &repo.Registry{ID: uuid.New().String(), Name: strings.TrimSpace(in.Name),
		URL: strings.TrimSpace(in.URL), Username: in.Username, Password: in.Password,
		CreatedAt: time.Now().UTC()}
	if err := a.store.CreateRegistry(c.Request().Context(), r); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, toRegistryOut(r))
}

// patchRegistry sets the managed registry's TLS domain, which is what lets a
// worker pull without an insecure-registries entry. Changing it rewrites the
// traefik route.
func (a *API) patchRegistry(c echo.Context) error {
	ctx := c.Request().Context()
	r, err := a.store.GetRegistry(ctx, c.Param("id"))
	if err != nil || r == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	var in registryPatch
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if in.Username != nil {
		r.Username = *in.Username
	}
	if in.Password != nil {
		r.Password = *in.Password
	}
	domain := r.Domain
	if in.Domain != nil {
		domain = *in.Domain
	}
	if r.Managed {
		// SetRegistryDomain also runs registry.EnsureManaged, which the panel
		// did and this path did not: traefik routed to a "registry" alias the
		// service was not carrying.
		if err := a.px.SetRegistryDomain(ctx, r, domain); err != nil {
			return err
		}
	} else {
		r.Domain = strings.TrimSpace(domain)
		if err := a.store.UpdateRegistry(ctx, r); err != nil {
			return err
		}
	}
	return c.JSON(http.StatusOK, toRegistryOut(r))
}

func (a *API) deleteRegistry(c echo.Context) error {
	ctx := c.Request().Context()
	r, err := a.store.GetRegistry(ctx, c.Param("id"))
	if err != nil || r == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if r.Managed {
		// Every build pushes to it, so a deploy would have nowhere to get its
		// image from. Clearing its domain is the thing a caller can do.
		return echo.NewHTTPError(http.StatusConflict, "the managed registry cannot be removed")
	}
	if err := a.store.DeleteRegistry(ctx, r.ID); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func toRegistryOut(r *repo.Registry) registryOut {
	return registryOut{ID: r.ID, Name: r.Name, URL: r.URL, Domain: r.Domain,
		Username: r.Username, Managed: r.Managed}
}
