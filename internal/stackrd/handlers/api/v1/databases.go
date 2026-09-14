package v1

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envutil"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func (a *API) listDBs(c echo.Context) error {
	ds, err := a.store.ListTiles(c.Request().Context())
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
		s, err := a.store.GetStack(ctx, d.StackID)
		if err != nil || s == nil {
			return ""
		}
		org, err := a.store.GetOrg(ctx, s.OrgID)
		if err != nil || org == nil {
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
		env, err := a.store.GetEnvironment(ctx, d.EnvironmentID)
		if err != nil || env == nil {
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

// applyScope resolves a scope name onto the tile. ScopeID is derived from the
// stack, never taken from the caller, so no request can point an instance at
// another tenant.
func applyScope(d *repo.Tile, s *repo.Stack, scope string) error {
	switch scope {
	case "", "env":
		d.ScopeKind, d.ScopeID = "env", ""
	case "stack":
		d.ScopeKind, d.ScopeID = "stack", s.ID
	case "org":
		d.ScopeKind, d.ScopeID = "org", s.OrgID
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "scope must be env, stack, or org")
	}
	return nil
}

// patchDB updates the instance settings the canvas exposes: the published
// port, the sharing scope, and the resource limits. All three are config-modeled,
// so rejectManaged keeps a file-owned stack from drifting.
func (a *API) patchDB(c echo.Context) error {
	ctx := c.Request().Context()
	t, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	if !t.IsManaged() {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err := a.rejectManaged(ctx, t.StackID); err != nil {
		return err
	}
	var in dbPatch
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	redeploy := false
	if in.ExternalPort != nil {
		if *in.ExternalPort < 0 || *in.ExternalPort > 65535 {
			return echo.NewHTTPError(http.StatusBadRequest, "external_port out of range")
		}
		redeploy = redeploy || *in.ExternalPort != t.ExternalPort
		t.ExternalPort = *in.ExternalPort
	}
	if in.CPULimit != nil {
		if *in.CPULimit < 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "cpu_limit must not be negative")
		}
		redeploy = redeploy || *in.CPULimit != t.CPULimit
		t.CPULimit = *in.CPULimit
	}
	if in.MemLimitMB != nil {
		mem := *in.MemLimitMB
		if mem < 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "mem_limit_mb must not be negative")
		}
		if mem > 0 && mem < 6 {
			mem = 6 // docker rejects memory caps under 6MB
		}
		redeploy = redeploy || mem != t.MemLimitMB
		t.MemLimitMB = mem
	}
	if in.Image != nil {
		img := *in.Image
		if img == "" {
			img = managedtiles.Engines[t.Engine].DefaultImage
		}
		redeploy = redeploy || img != t.ImageRef
		t.ImageRef = img
	}
	if in.ShmSizeMB != nil {
		if *in.ShmSizeMB < 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "shm_size_mb must not be negative")
		}
		redeploy = redeploy || *in.ShmSizeMB != t.ShmSizeMB
		t.ShmSizeMB = *in.ShmSizeMB
	}
	if in.Scope != nil {
		s, serr := a.store.GetStack(ctx, t.StackID)
		if serr != nil || s == nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		if err := applyScope(t, s, *in.Scope); err != nil {
			return err
		}
		// No redeploy: scope governs who may provision from the instance, and
		// the running container knows nothing about it.
	}
	if in.UpdatePolicy != nil {
		switch *in.UpdatePolicy {
		case "", "off":
			t.UpdatePolicy = "off"
		case "notify", "auto":
			t.UpdatePolicy = *in.UpdatePolicy
		default:
			return echo.NewHTTPError(http.StatusBadRequest, "update_policy must be off, notify or auto")
		}
		// No redeploy: the watcher reads the row.
	}
	t.UpdatedAt = time.Now().UTC()
	if err := a.store.UpdateTile(ctx, t); err != nil {
		return err
	}
	// The port mapping and the resource caps live on the container, so the row
	// alone changes nothing until it is recreated.
	if redeploy && t.Status == "running" {
		if derr := managedtiles.NewService(a.clus, a.store).Deploy(ctx, t); derr != nil {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, "settings saved but redeploy failed: "+derr.Error())
		}
	}
	return c.JSON(http.StatusOK, toDBOut(t))
}

func (a *API) getDB(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), false)
	if err != nil {
		return err
	}
	if !t.IsManaged() {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	return c.JSON(http.StatusOK, toDBOut(t))
}

func (a *API) createDB(c echo.Context) error {
	s, err := a.requireStackAccess(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := a.requireOrgWrite(ctx, c, s.OrgID); err != nil {
		return err
	}
	if err := managedGuard(s); err != nil {
		return err
	}
	var in dbIn
	if err := c.Bind(&in); err != nil || in.Name == "" || in.Engine == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name and engine required")
	}
	env, err := a.resolveEnv(ctx, s.ID, in.EnvSlug)
	if err != nil {
		return err
	}
	slug := repo.Slugify(in.Name)
	if repo.ReservedSlug(slug) {
		return echo.NewHTTPError(http.StatusBadRequest, "\""+slug+"\" is reserved for variable references; pick another name")
	}
	if existing, _ := a.store.GetTileBySlug(ctx, env.ID, slug); existing != nil {
		return echo.NewHTTPError(http.StatusConflict, "a tile with that name already exists in the environment")
	}
	now := time.Now().UTC()
	d := &repo.Tile{ID: uuid.New().String(), StackID: s.ID, EnvironmentID: env.ID,
		Name: in.Name, Slug: slug, Engine: in.Engine, SourceType: "image", Kind: "service",
		WebhookToken: uuid.New().String(), Status: "idle", CreatedAt: now, UpdatedAt: now}
	if err := applyScope(d, s, in.Scope); err != nil {
		return err
	}
	if err := managedtiles.NewDB(d); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := a.store.CreateTile(ctx, d); err != nil {
		return err
	}
	managedtiles.PublishConnection(ctx, a.store, d)
	svc := managedtiles.NewService(a.clus, a.store)
	if err := svc.Deploy(ctx, d); err == nil {
		if serr := a.store.UpdateTileStatus(ctx, d.ID, "running"); serr != nil {
			slog.Error("database status not saved", "tile", d.ID, "status", "running", "error", serr)
		}
		d.Status = "running"
	} else {
		if serr := a.store.UpdateTileStatus(ctx, d.ID, "error"); serr != nil {
			slog.Error("database status not saved", "tile", d.ID, "status", "error", "error", serr)
		}
		d.Status = "error"
	}
	out := toDBOut(d)
	if org, _ := a.store.GetOrg(ctx, s.OrgID); org != nil {
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
	consumer, err := a.requireTile(c, c.Param("id"), true)
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
		instance, err = a.store.GetTile(ctx, in.InstanceID)
	}
	if err != nil || instance == nil || !managedtiles.Eligible(ctx, a.store, instance, consumer) {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid shared instance")
	}
	p, err := managedtiles.NewService(a.clus, a.store).Provision(ctx, instance, consumer, in.Name, in.Public)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := a.wireProvisionFor(ctx, instance, consumer, p, in.EnvVar); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, toProvisionOut(p))
}

// attachProvision points the consumer app at an existing logical database
// (same env) so it can share another tile's database.
func (a *API) attachProvision(c echo.Context) error {
	consumer, err := a.requireTile(c, c.Param("id"), true)
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
	if err != nil || src == nil || src.EnvID != consumer.EnvironmentID {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid database")
	}
	instance, err := a.store.GetTile(ctx, src.InstanceTileID)
	if err != nil || instance == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "instance not found")
	}
	p, err := managedtiles.NewService(a.clus, a.store).AttachExisting(ctx, instance, src, consumer)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := a.wireProvisionFor(ctx, instance, consumer, p, in.EnvVar); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, toProvisionOut(p))
}

// listAppProvisions lists the logical databases a consumer app holds.
func (a *API) listAppProvisions(c echo.Context) error {
	consumer, err := a.requireTile(c, c.Param("id"), false)
	if err != nil {
		return err
	}
	ps, err := a.store.ListProvisionsByConsumer(c.Request().Context(), consumer.ID)
	if err != nil {
		return err
	}
	out := make([]provisionOut, 0, len(ps))
	for i := range ps {
		out = append(out, toProvisionOut(&ps[i]))
	}
	return c.JSON(http.StatusOK, out)
}

// wireProvisionFor wires a provision's secret(s) into the consumer's env and
// redeploys. Engines whose slices need more than a url (s3: endpoint, bucket,
// keys) auto-inject their whole output set and ignore envVar; the rest inject
// the single url secret under envVar.
func (a *API) wireProvisionFor(ctx context.Context, instance, consumer *repo.Tile, p *repo.Provision, envVar string) error {
	// Config-managed stack: the file owns consumer.Env, so an injection here is
	// stripped by the next apply. Provision + publish only; the response carries
	// the secret name for the user to reference from their config file.
	if s, err := a.store.GetStack(ctx, consumer.StackID); err != nil || s == nil || s.ConfigManaged() {
		return err
	}
	if managedtiles.Engines[instance.Engine].AutoInjectAll {
		for k, v := range managedtiles.NewService(a.clus, a.store).AutoInjectVars(ctx, instance, p) {
			consumer.Env, _ = envutil.Inject(consumer.Env, k, v)
		}
		if err := a.store.UpdateTile(ctx, consumer); err != nil {
			return err
		}
		_, err := a.engine.Enqueue(ctx, consumer, "provision")
		return err
	}
	return a.wireProvision(ctx, consumer, managedtiles.Ref(instance, p, managedtiles.DefaultOutput(instance.Engine)), envVar)
}

// wireProvision injects a reference to the provisioned resource into the
// consumer's env under envVar (blank = publish only) and redeploys so it joins
// the shared network.
func (a *API) wireProvision(ctx context.Context, consumer *repo.Tile, ref, envVar string) error {
	if envVar == "" {
		return nil // resource published; the user writes the reference themselves
	}
	consumer.Env, _ = envutil.Inject(consumer.Env, envVar, ref)
	if err := a.store.UpdateTile(ctx, consumer); err != nil {
		return err
	}
	_, err := a.engine.Enqueue(ctx, consumer, "provision")
	return err
}

func (a *API) deleteDB(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	if !t.IsManaged() {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	ctx := c.Request().Context()
	if err := a.rejectManaged(ctx, t.StackID); err != nil {
		return err
	}
	// Deleting a shared instance destroys every logical database or bucket cut
	// from it, and each consumer keeps a variable pointing at a resource that no
	// longer resolves, so the deploy that discovers it is the next one, not
	// this call. Refuse by default; the caller has to say it means it.
	if c.QueryParam("force") != "true" {
		held, err := a.store.ListProvisionsByInstance(ctx, t.ID)
		if err != nil {
			return err
		}
		if len(held) > 0 {
			return echo.NewHTTPError(http.StatusConflict,
				fmt.Sprintf("%d consumer(s) hold slices on this instance; detach them first, or pass force=true to destroy the data with it", len(held)))
		}
	}
	if err := a.teardownTile(ctx, t); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
