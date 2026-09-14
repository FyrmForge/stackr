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

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// requireOrgRegistry resolves the org and the managed registry together: every
// route here needs both, and a missing registry is a 503 rather than a 404,
// because the org is fine and the thing that is down is ours.
func (a *API) requireOrgRegistry(c echo.Context, write bool) (*repo.Org, *repo.Registry, error) {
	o, err := a.requireOrg(c, c.Param("id"))
	if err != nil {
		return nil, nil, err
	}
	if write {
		if err := a.requireOrgWrite(c.Request().Context(), c, o.ID); err != nil {
			return nil, nil, err
		}
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
	o, err := a.requireOrg(c, c.Param("id"))
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
	if err := c.Bind(&in); err != nil || strings.TrimSpace(in.Name) == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name required")
	}
	name := strings.TrimSpace(in.Name)
	if name == registry.SystemCredentialName {
		return echo.NewHTTPError(http.StatusConflict,
			"\""+registry.SystemCredentialName+"\" is the credential stackr's own deploys use; pick another name")
	}
	secret := secrets.RandomHex(24)
	cred := &repo.OrgRegistryCredential{ID: uuid.New().String(), OrgID: o.ID, Name: name,
		SecretHash: registry.HashSecret(secret), Prefix: secret[:8], CreatedAt: time.Now().UTC()}
	if err := a.store.CreateOrgRegistryCredential(c.Request().Context(), cred); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, toRegistryCredentialOut(cred, secret))
}

func (a *API) deleteRegistryCredential(c echo.Context) error {
	ctx := c.Request().Context()
	cred, err := a.store.GetOrgRegistryCredential(ctx, c.Param("cred"))
	if err != nil || cred == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	o, err := a.requireOrg(c, c.Param("id"))
	if err != nil {
		return err
	}
	if cred.OrgID != o.ID {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err := a.requireOrgWrite(ctx, c, o.ID); err != nil {
		return err
	}
	if cred.System {
		return echo.NewHTTPError(http.StatusConflict,
			"this is the credential stackr's own deploys push with; removing it would break the next build")
	}
	if err := a.store.DeleteOrgRegistryCredential(ctx, cred.ID); err != nil {
		return err
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
func (a *API) orgImage(c echo.Context, o *repo.Org) (string, error) {
	name, err := url.PathUnescape(c.Param("name"))
	if err != nil {
		return "", echo.NewHTTPError(http.StatusBadRequest, "malformed image name")
	}
	ns := registry.Namespace(o.Slug)
	// A short name is the friendly form the listing shows; a full one is what
	// a script copies out of an image reference. Both resolve, neither can
	// reach outside the namespace.
	if !strings.HasPrefix(name, ns) {
		name = ns + name
	}
	if !strings.HasPrefix(name, ns) || !registry.ValidRepoName(name) {
		// Not the registry client's error: that one comes back as a 502 and
		// repeats the namespaced name, which reads as "the registry is broken"
		// for what is a bad path from the caller.
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
	if used, where := a.tagInUse(c, o, name, tag); used {
		return echo.NewHTTPError(http.StatusConflict, "still deployed on "+where)
	}
	if err := registry.NewClient(reg, a.signer).DeleteTag(ctx, name, tag); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// tagInUse reports whether any tile in the org runs this image reference, and
// names the first one that does.
//
// walks the org's stacks and tiles. Tens of rows; add a store query
// if an org ever grows big enough to notice.
func (a *API) tagInUse(c echo.Context, o *repo.Org, name, tag string) (bool, string) {
	ctx := c.Request().Context()
	stacks, err := a.store.ListStacksByOrg(ctx, o.ID)
	if err != nil {
		return false, ""
	}
	for _, st := range stacks {
		tiles, err := a.store.ListTilesByStack(ctx, st.ID)
		if err != nil {
			continue
		}
		for i := range tiles {
			ds, err := a.store.ListDeploymentsByTile(ctx, tiles[i].ID, 5)
			if err != nil {
				continue
			}
			for _, d := range ds {
				if d.Status != "done" || d.ImageTag == "" {
					continue
				}
				// The stored tag carries the pull host; the registry path is
				// its tail, which is what identifies the image.
				if strings.HasSuffix(d.ImageTag, "/"+name+":"+tag) || d.ImageTag == name+":"+tag {
					return true, st.Slug + "/" + tiles[i].Slug
				}
			}
		}
	}
	return false, ""
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
	if in.Domain != nil {
		r.Domain = strings.TrimSpace(*in.Domain)
	}
	if in.Username != nil {
		r.Username = *in.Username
	}
	if in.Password != nil {
		r.Password = *in.Password
	}
	if err := a.store.UpdateRegistry(ctx, r); err != nil {
		return err
	}
	if r.Managed && a.px != nil {
		if err := a.px.WriteRegistry(r.Domain); err != nil {
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
