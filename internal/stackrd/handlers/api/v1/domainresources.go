package v1

// Domain resources: hosts owned at a level (instance/org/stack) that tiles
// generate per-environment hostnames under. See envops.AutoHost.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func toDomainResourceOut(r *repo.DomainResource) domainResourceOut {
	return domainResourceOut{ID: r.ID, Level: r.Level, OwnerID: r.OwnerID, Host: r.Host,
		IncludeEnvOnDefault: r.IncludeEnvOnDefault, ACMEEmail: r.ACMEEmail}
}

// listDomainResources returns the resources the caller can claim under:
// instance rows for everyone, org/stack rows only within the caller's orgs.
func (a *API) listDomainResources(c echo.Context) error {
	ctx := c.Request().Context()
	all, err := a.store.ListDomainResources(ctx)
	if err != nil {
		return err
	}
	out := make([]domainResourceOut, 0, len(all))
	for i := range all {
		r := &all[i]
		switch r.Level {
		case "org":
			if !a.orgAllowed(c, r.OwnerID) {
				continue
			}
		case "stack":
			s, err := a.store.GetStack(ctx, r.OwnerID)
			if err != nil || s == nil || !a.orgAllowed(c, s.OrgID) {
				continue
			}
		}
		out = append(out, toDomainResourceOut(r))
	}
	return c.JSON(http.StatusOK, out)
}

// resolveResourceTenancy checks the caller may write a resource at level/owner
// and returns the canonical owner id. Instance level is admin-only; org and
// stack owners must sit inside the caller's own orgs with a write role.
func (a *API) resolveResourceTenancy(c echo.Context, level, owner string) (string, error) {
	ctx := c.Request().Context()
	switch level {
	case "instance", "node":
		// One level, two names: the owner is a node, and "local" is the single
		// node a non-clustered install has. Callers that think in nodes should
		// not have to learn that stackr calls it the instance level.
		if !a.isAdmin(c) {
			return "", echo.NewHTTPError(http.StatusForbidden, "instance-level domain resources are admin-only")
		}
		if owner == "" {
			owner = "local"
		}
		return owner, nil
	case "org":
		if owner == "" {
			return "", echo.NewHTTPError(http.StatusBadRequest, "owner (org id) required for org level")
		}
		// requireOrg accepts an id or an org slug, and gives the same 409 a
		// half-set-up org gets everywhere else; a bare 404 here would say "no
		// such org" about one the caller owns.
		o, err := a.requireOrg(c, owner)
		if err != nil {
			return "", err
		}
		if err := a.requireOrgWrite(ctx, c, o.ID); err != nil {
			return "", err
		}
		return o.ID, nil
	case "stack":
		if owner == "" {
			return "", echo.NewHTTPError(http.StatusBadRequest, "owner (stack id) required for stack level")
		}
		s, err := a.requireStackAccess(c, owner)
		if err != nil {
			return "", err
		}
		if err := a.requireOrgWrite(ctx, c, s.OrgID); err != nil {
			return "", err
		}
		return s.ID, nil
	default:
		return "", echo.NewHTTPError(http.StatusBadRequest, "level must be instance (or node), org or stack")
	}
}

func (a *API) createDomainResource(c echo.Context) error {
	ctx := c.Request().Context()
	var in domainResourceIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	host := strings.TrimSpace(in.Host)
	if err := envops.ValidateResourceHost(host); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	level := in.Level
	// "node" is an accepted spelling of the same level, normalized here so only
	// one value is ever stored; the rest of the codebase knows "instance".
	if level == "" || level == "node" {
		level = "instance"
	}
	owner, err := a.resolveResourceTenancy(c, level, in.Owner)
	if err != nil {
		return err
	}
	all, err := a.store.ListDomainResources(ctx)
	if err != nil {
		return err
	}
	if envops.HostTaken(all, host) {
		return echo.NewHTTPError(http.StatusConflict, "that host is already a domain resource")
	}
	if err := a.rejectManagedOwner(ctx, level, owner); err != nil {
		return err
	}
	r := &repo.DomainResource{ID: uuid.New().String(), Level: level, OwnerID: owner, Host: host,
		IncludeEnvOnDefault: in.IncludeEnvOnDefault, CreatedAt: time.Now().UTC()}
	if err := a.store.CreateDomainResource(ctx, r); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, toDomainResourceOut(r))
}

func (a *API) deleteDomainResource(c echo.Context) error {
	ctx := c.Request().Context()
	all, err := a.store.ListDomainResources(ctx)
	if err != nil {
		return err
	}
	for i := range all {
		r := &all[i]
		if r.ID != c.Param("id") {
			continue
		}
		// Same write rules as creation, deleting is editing the owner's scope.
		if _, err := a.resolveResourceTenancy(c, r.Level, r.OwnerID); err != nil {
			return err
		}
		if err := a.rejectManagedOwner(ctx, r.Level, r.OwnerID); err != nil {
			return err
		}
		if err := a.store.DeleteDomainResource(ctx, r.ID); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
	return echo.NewHTTPError(http.StatusNotFound, "not found")
}

// rejectManagedOwner blocks a domain-resource write when the owner's config
// file declares domains:. Instance-level rows have no config file behind them.
func (a *API) rejectManagedOwner(ctx context.Context, level, owner string) error {
	switch level {
	case "stack":
		s, err := a.store.GetStack(ctx, owner)
		if err != nil || s == nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		return managedGuard(s)
	case "org":
		o, err := a.store.GetOrg(ctx, owner)
		if err != nil || o == nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		if o.ConfigManaged() {
			return echo.NewHTTPError(http.StatusConflict, "organization is managed by "+o.ConfigRepo+"; declare domains: in the org config file")
		}
	}
	return nil
}

// patchDomainResource changes the Let's Encrypt account certificates under a
// host are issued on. Nothing else: the host is the resource's identity, and
// moving it would orphan every generated name nested under it.
//
// Traefik restarts once when a new address appears, because the account is in
// its static config.
func (a *API) patchDomainResource(c echo.Context) error {
	ctx := c.Request().Context()
	all, err := a.store.ListDomainResources(ctx)
	if err != nil {
		return err
	}
	var res *repo.DomainResource
	for i := range all {
		if all[i].ID == c.Param("id") {
			res = &all[i]
			break
		}
	}
	if res == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if _, err := a.resolveResourceTenancy(c, res.Level, res.OwnerID); err != nil {
		return err
	}
	var in domainResourcePatch
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if in.ACMEEmail != nil {
		email := strings.ToLower(strings.TrimSpace(*in.ACMEEmail))
		if err := a.store.SetDomainResourceACME(ctx, res.ID, email); err != nil {
			return err
		}
		res.ACMEEmail = email
		if a.px != nil {
			// The account lives in the static config, so the resolver set has
			// to be rewritten before the next certificate is asked for.
			_ = a.px.EnsureTraefik(ctx)
		}
	}
	return c.JSON(http.StatusOK, toDomainResourceOut(res))
}
