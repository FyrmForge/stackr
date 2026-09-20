package v1

// Path resolution and single-slice operations. The CLI addresses infrastructure
// by path (org:stack:env:slug, or relative to a linked directory) while every
// other route is keyed by id, so one endpoint turns the former into the latter
// and the rest of the surface stays as it was.

import (
	"net/http"

	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// resolvePath answers "what does this address name, and what is its id".
//
// Bounded to the caller's orgs by ResolveScope: this turns a name into an id,
// so an unbounded version would let any key confirm another org's stacks and
// databases exist by guessing at their names.
func (a *API) resolvePath(c echo.Context) error {
	var in resolveIn
	if err := c.Bind(&in); err != nil || in.Path == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "path required")
	}
	scope := managedtiles.ResolveScope{AllowedOrgs: a.userOrgIDs(c)}
	if in.EnvID != "" {
		// The linked env enables relative paths, but only after confirming the
		// caller may see it, or it becomes a way to resolve names inside
		// someone else's environment.
		// requireEnvAccess, NOT the bare loader: this route is KindDeferred,
		// so the route's gate resolved no tenancy and checked nothing. The
		// membership check has to be here or it happens nowhere.
		if _, err := a.requireEnvAccess(c, in.EnvID); err != nil {
			return err
		}
		scope.EnvID = in.EnvID
	}
	t, err := managedtiles.ResolveTarget(c.Request().Context(), a.store, in.Path, scope)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	out := resolveOut{Kind: t.Kind, Path: t.Path, InstanceID: t.Instance.ID, Name: t.Instance.Slug}
	if t.Kind == managedtiles.TargetSlice {
		out.ID = t.Provision.ID
		out.Name = t.Provision.DBName
	} else {
		out.ID = t.Instance.ID
	}
	return c.JSON(http.StatusOK, out)
}

// listInstanceProvisions lists the slices cut from one shared instance, the
// listing the web drawer has always had and the API never did. Carries no
// passwords: this exists to resolve and enumerate, and credentials are read
// through the resource outputs with secrets:read.
func (a *API) listInstanceProvisions(c echo.Context) error {
	inst, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	if !inst.IsManaged() {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	ps, err := a.store.ListProvisionsByInstance(c.Request().Context(), inst.ID)
	if err != nil {
		return err
	}
	out := make([]sliceOut, 0, len(ps))
	for i := range ps {
		out = append(out, a.toSliceOut(c, inst, &ps[i]))
	}
	return c.JSON(http.StatusOK, out)
}

// createInstanceProvision cuts a slice with no consumer attached.
//
// The row shape is the one config-declared slices already have, active, no
// ConsumerTileID, so nothing downstream treats it as a special case. It lands
// in the instance's own environment unless the caller names another, which a
// stack- or org-scoped instance needs since it serves several.
func (a *API) createInstanceProvision(c echo.Context) error {
	var in sliceIn
	if err := c.Bind(&in); err != nil || in.Name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name required")
	}
	inst, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	if !inst.IsManaged() {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	// Which environment the slice belongs to decides who can ever consume it:
	// AttachExisting refuses across environments, so a slice cut into the wrong
	// one is a database nothing can use. A stack- or org-scoped instance serves
	// several environments, so the caller's own is the meaningful default and
	// the instance's is only the fallback.
	envID := in.EnvID
	if envID == "" {
		envID = inst.EnvironmentID
	}
	env, err := a.env(c, envID)
	if err != nil {
		return err
	}
	slug := in.Slug
	if slug == "" {
		slug = in.Name
	}
	// Cut carries the checks this path never had: the sharing scope, the
	// readiness wait (one call can create an instance and its slices
	// together), and adopt-rather-than-uniquify, so asking twice for the same
	// slice does not leave a silent second copy of it.
	p, err := a.slices.Cut(c.Request().Context(), inst, env, slug, in.Name, in.Public, false)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusCreated, a.toSliceOut(c, inst, p))
}

// forkProvision copies a live slice into a fresh one on the same instance, so
// a migration can be rehearsed against real data without risking the original.
//
// Its own scope: forking reads every byte of the source and writes a second
// copy of it, which is not what "create a database" grants.
func (a *API) forkProvision(c echo.Context) error {
	var in forkIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	ctx := c.Request().Context()
	src, err := a.store.GetProvision(ctx, c.Param("id"))
	if err != nil || src == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	inst, err := a.tile(c, src.InstanceTileID)
	if err != nil {
		return err
	}
	fork, err := a.slices.Fork(ctx, inst, src, in.Slug, in.Name)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusCreated, a.toSliceOut(c, inst, fork))
}

// deleteProvision destroys one slice: the logical database or bucket itself,
// and every consumer's row pointing at it.
//
// That breadth is why the CLI prompts with the consumer list first. It is not
// incidental, the slice is one object shared by those rows, so dropping it
// necessarily unhooks all of them. Detaching a single consumer is a different
// operation.
func (a *API) deleteProvision(c echo.Context) error {
	ctx := c.Request().Context()
	p, err := a.store.GetProvision(ctx, c.Param("id"))
	if err != nil || p == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	inst, err := a.tile(c, p.InstanceTileID)
	if err != nil {
		return err
	}
	if err := a.slices.Drop(ctx, inst, p.DBName); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// toSliceOut describes a slice by its address and consumers, so a caller can
// see what removing it would take with it.
func (a *API) toSliceOut(c echo.Context, inst *repo.Tile, p *repo.Provision) sliceOut {
	ctx := c.Request().Context()
	out := sliceOut{
		ID:         p.ID,
		InstanceID: p.InstanceTileID,
		Name:       p.DBName,
		Slug:       managedtiles.ResourceSlug(inst, p),
		Status:     p.Status,
		Public:     p.Public,
	}
	for _, row := range a.slices.Rows(ctx, inst.ID, p.DBName) {
		if row.ConsumerTileID == "" {
			continue
		}
		if t, _ := a.tiles.Get(ctx, row.ConsumerTileID); t != nil {
			out.Consumers = append(out.Consumers, t.Name)
		}
	}
	return out
}

// requireEnvAccess loads an environment and 404s unless the caller's org owns
// the stack it belongs to.
func (a *API) requireEnvAccess(c echo.Context, envID string) (*repo.Environment, error) {
	ctx := c.Request().Context()
	env, err := a.envs.Get(ctx, envID)
	if err != nil {
		env, err = a.envByPath(ctx, envID) // org:stack:env
	}
	if err != nil || env == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if _, err := a.stack(c, env.StackID); err != nil {
		return nil, err
	}
	return env, nil
}
