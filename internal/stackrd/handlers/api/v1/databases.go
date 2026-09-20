package v1

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func (a *API) listDBs(c echo.Context) error {
	ds, err := a.tiles.ListAll(c.Request().Context())
	if err != nil {
		return err
	}
	out := make([]dbOut, 0, len(ds))
	// The colon-path is what every other verb addresses an instance by
	// (`uses:` in a config file, `tile provision`), and until now it was
	// returned only by create, so a caller who did not keep the creation
	// response had no way to get it back.
	paths := map[string]string{}
	for i := range ds {
		if !ds[i].IsManaged() || !a.orgAllowed(c, a.stackOrg(c, ds[i].StackID)) {
			continue
		}
		o := toDBOut(&ds[i])
		o.Path = a.infraPath(c.Request().Context(), &ds[i], paths)
		out = append(out, o)
	}
	return c.JSON(http.StatusOK, out)
}

// infraPath builds an instance's colon-path address, memoising the stack lookup
// across a listing (every instance in a stack shares the org and stack slugs).
func (a *API) infraPath(ctx context.Context, d *repo.Tile, cache map[string]string) string {
	prefix, ok := cache[d.StackID]
	if !ok {
		s, err := a.stacks.Get(ctx, d.StackID)
		if err != nil {
			return ""
		}
		org, err := a.orgs.Get(ctx, s.OrgID)
		if err != nil {
			return ""
		}
		prefix = org.Slug + "\x00" + s.Slug
		cache[d.StackID] = prefix
	}
	orgSlug, stackSlug, _ := strings.Cut(prefix, "\x00")
	envSlug := ""
	// Only an env-scoped instance carries the env segment, so the lookup is
	// skipped for the other two scopes rather than done and discarded.
	if d.ScopeKind == "" || d.ScopeKind == "env" {
		env, err := a.envs.Get(ctx, d.EnvironmentID)
		if err != nil {
			return ""
		}
		envSlug = env.Slug
	}
	return managedtiles.InfraPath(d.ScopeKind, orgSlug, stackSlug, envSlug, d.Slug)
}

func toDBOut(d *repo.Tile) dbOut {
	scope := d.ScopeKind
	if scope == "" {
		scope = "env"
	}
	return dbOut{ID: d.ID, StackID: d.StackID, Name: d.Name, Engine: d.Engine,
		Status: d.Status, ExternalPort: d.ExternalPort, Scope: scope,
		CPULimit: d.CPULimit, MemLimitMB: d.MemLimitMB,
		Image: d.ImageRef, ShmSizeMB: d.ShmSizeMB,
		UpdatePolicy: d.UpdatePolicy, ImageDigest: d.ImageDigest, LatestDigest: d.LatestDigest}
}

// patchDB updates the instance settings the canvas exposes: the published
// port, the sharing scope, the resource limits, the image and the update
// policy. The merge is the edge's job (D6): every field is optional, so an
// absent key must leave the row alone rather than zero it.
func (a *API) patchDB(c echo.Context) error {
	ctx := c.Request().Context()
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	if !t.IsManaged() {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	var in dbPatch
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	if in.ExternalPort != nil {
		t.ExternalPort = *in.ExternalPort
	}
	if in.CPULimit != nil {
		t.CPULimit = *in.CPULimit
	}
	if in.MemLimitMB != nil {
		t.MemLimitMB = *in.MemLimitMB
	}
	if in.Image != nil {
		t.ImageRef = *in.Image
	}
	if in.ShmSizeMB != nil {
		t.ShmSizeMB = *in.ShmSizeMB
	}
	if in.UpdatePolicy != nil {
		t.UpdatePolicy = *in.UpdatePolicy
	}
	// Scope asks its own gate — it is the one field the org-scope exception
	// does not cover — but it does not write: one merged row, one write, one
	// diff, or the settings beside it would land with no redeploy behind them.
	if in.Scope != nil {
		if err := a.instances.PlanScope(ctx, t, *in.Scope); err != nil {
			return stackrmw.HTTP(err)
		}
	}
	if err := a.instances.Update(ctx, t, a.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusOK, toDBOut(t))
}

func (a *API) getDB(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	if !t.IsManaged() {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	// The colon-path, which listDBs sets and this used to omit, so a caller
	// that read one instance back could not address it.
	out := toDBOut(t)
	out.Path = a.infraPath(c.Request().Context(), t, map[string]string{})
	return c.JSON(http.StatusOK, out)
}

func (a *API) createDB(c echo.Context) error {
	s, err := a.stack(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var in dbIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	env, err := a.resolveEnv(ctx, s.ID, in.EnvSlug)
	if err != nil {
		return err
	}
	d := &repo.Tile{StackID: s.ID, EnvironmentID: env.ID, Name: in.Name, Engine: in.Engine}
	// The gate, the slug rules, the scope derivation, the generated
	// credentials and the deploy are all the service's; what stays here is
	// the tenancy of the request and the wire shape of the answer.
	if _, err := a.instances.Create(ctx, d, in.Scope, nil, a.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	out := toDBOut(d)
	if org, _ := a.orgs.Get(ctx, s.OrgID); org != nil {
		out.Path = managedtiles.InfraPath(d.ScopeKind, org.Slug, s.Slug, env.Slug, d.Slug)
	}
	return c.JSON(http.StatusCreated, out)
}

// --- provisioning (shared instances) ---

func toProvisionOut(p *repo.Provision) provisionOut {
	return provisionOut{ID: p.ID, InstanceID: p.InstanceTileID, ConsumerID: p.ConsumerTileID,
		DBName: p.DBName, Secret: p.SecretName, Status: p.Status}
}

// provisionApp creates a logical database in a shared instance for the consumer
// app, wires the connection secret into the app's env, and redeploys it.
func (a *API) provisionApp(c echo.Context) error {
	consumer, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	if consumer.IsManaged() || consumer.IsVolume() {
		return echo.NewHTTPError(http.StatusBadRequest, "only services and crons can consume provisions")
	}
	ctx := c.Request().Context()
	var in provisionIn
	if err := c.Bind(&in); err != nil || (in.InstanceID == "" && in.InfraPath == "") {
		return echo.NewHTTPError(http.StatusBadRequest, "instance_id or infra_path required")
	}
	var instance *repo.Tile
	if in.InfraPath != "" {
		// Scoped to the caller's orgs and to the consumer's env, so a path is
		// resolved on the same terms as everywhere else rather than reaching
		// across tenants.
		var t *managedtiles.InfraTarget
		t, err = managedtiles.ResolveTarget(ctx, a.store, in.InfraPath, managedtiles.ResolveScope{
			AllowedOrgs: a.userOrgIDs(c), EnvID: consumer.EnvironmentID,
		})
		if err == nil && t.Kind != managedtiles.TargetInstance {
			return echo.NewHTTPError(http.StatusBadRequest, "infra_path must name a shared instance, not a slice")
		}
		if t != nil {
			instance = t.Instance
		}
	} else {
		instance, err = a.tiles.Get(ctx, in.InstanceID)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid shared instance")
	}
	p, err := a.slices.Provision(ctx, instance, consumer, in.Name, in.Public)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if _, err := a.slices.Wire(ctx, instance, consumer, p, in.EnvVar); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusCreated, toProvisionOut(p))
}

// attachProvision points the consumer app at an existing logical database
// (same env) so it can share another tile's database.
func (a *API) attachProvision(c echo.Context) error {
	consumer, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	if consumer.IsManaged() || consumer.IsVolume() {
		return echo.NewHTTPError(http.StatusBadRequest, "only services and crons can consume provisions")
	}
	ctx := c.Request().Context()
	var in attachIn
	if err := c.Bind(&in); err != nil || in.ProvisionID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "provision_id required")
	}
	src, err := a.store.GetProvision(ctx, in.ProvisionID)
	if err != nil {
		return err
	}
	p, instance, err := a.slices.Attach(ctx, src, consumer)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if _, err := a.slices.Wire(ctx, instance, consumer, p, in.EnvVar); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusCreated, toProvisionOut(p))
}

// listAppProvisions lists the logical databases a consumer app holds.
func (a *API) listAppProvisions(c echo.Context) error {
	consumer, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	ps, err := a.slices.ForConsumer(c.Request().Context(), consumer.ID)
	if err != nil {
		return err
	}
	out := make([]provisionOut, 0, len(ps))
	for i := range ps {
		out = append(out, toProvisionOut(&ps[i]))
	}
	return c.JSON(http.StatusOK, out)
}

func (a *API) deleteDB(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	if !t.IsManaged() {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	// Deleting a shared instance destroys every logical database or bucket cut
	// from it, and each consumer keeps a variable pointing at a resource that
	// no longer resolves. Refuse by default; the caller has to say it means it.
	force := c.QueryParam("force") == "true"
	if err := a.instances.Delete(c.Request().Context(), t, force, a.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.NoContent(http.StatusNoContent)
}
