package v1

// Domain resources: hosts owned at a level (instance/org/stack) that tiles
// generate per-environment hostnames under. See service.AutoHost.

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
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
	level := in.Level
	if level == "" || level == "node" {
		level = "instance"
	}
	// Who may write at this level is answered from the request's identity,
	// so it stays here. Everything about the host itself — its shape, whether
	// it is taken, whether it squats on another org's slug, whether the
	// owner's config file owns it, and the ACME account this path used to
	// accept and throw away — is the service's.
	owner, err := a.resolveResourceTenancy(c, level, in.Owner)
	if err != nil {
		return err
	}
	r, err := a.resources.Create(ctx, level, owner, in.Host, service.ResourceOpts{
		IncludeEnvOnDefault: in.IncludeEnvOnDefault,
		ACMEEmail:           in.ACMEEmail,
	})
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusCreated, toDomainResourceOut(r))
}

func (a *API) deleteDomainResource(c echo.Context) error {
	ctx := c.Request().Context()
	r, err := a.resources.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	// Same write rules as creation: deleting is editing the owner's scope.
	if _, err := a.resolveResourceTenancy(c, r.Level, r.OwnerID); err != nil {
		return err
	}
	if err := a.resources.Delete(ctx, r.ID); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.NoContent(http.StatusNoContent)
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
	res, err := a.resources.Get(ctx, c.Param("id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if _, err := a.resolveResourceTenancy(c, res.Level, res.OwnerID); err != nil {
		return err
	}
	var in domainResourcePatch
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	if in.ACMEEmail != nil {
		// The managed-owner refusal is inside SetACME. This path skipped it,
		// so a config-managed org's ACME account could be changed over the
		// API and reverted by the next apply.
		if err := a.resources.SetACME(ctx, res.ID, *in.ACMEEmail); err != nil {
			return stackrmw.HTTP(err)
		}
		res.ACMEEmail = strings.ToLower(strings.TrimSpace(*in.ACMEEmail))
	}
	return c.JSON(http.StatusOK, toDomainResourceOut(res))
}
