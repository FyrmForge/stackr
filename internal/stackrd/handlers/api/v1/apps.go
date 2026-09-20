package v1

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/runpolicy"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func (a *API) listApps(c echo.Context) error {
	proj := c.QueryParam("stack")
	env := c.QueryParam("env")
	apps, err := a.tiles.ListAll(c.Request().Context())
	if err != nil {
		return err
	}
	out := make([]appOut, 0, len(apps))
	for i := range apps {
		if apps[i].IsManaged() || apps[i].IsVolume() {
			continue
		}
		if proj != "" && apps[i].StackID != proj {
			continue
		}
		if env != "" && apps[i].EnvironmentID != env {
			continue
		}
		if !a.orgAllowed(c, a.stackOrg(c, apps[i].StackID)) {
			continue
		}
		out = append(out, toAppOut(&apps[i]))
	}
	return c.JSON(http.StatusOK, out)
}

func toAppOut(x *repo.Tile) appOut {
	return appOut{ID: x.ID, StackID: x.StackID, EnvID: x.EnvironmentID, Name: x.Name,
		SourceType: x.SourceType, GitURL: x.GitURL, GitBranch: x.GitBranch, Status: x.Status,
		UpdatePolicy: x.UpdatePolicy, WaitForCI: x.WaitForCI,
		ImageDigest: x.ImageDigest, LatestDigest: x.LatestDigest}
}

// createApp, patchApp and deleteApp write straight through, no staging. On a
// stack whose canvas edits queue into the pending set, the same change made
// here lands immediately; see docs/features/api.md. Deliberate
// (docs/plan-parity.md item 5), not an oversight.
func (a *API) createApp(c echo.Context) error {
	s, err := a.stack(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var in appIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	env, err := a.resolveEnv(ctx, s.ID, in.EnvSlug)
	if err != nil {
		return err
	}
	kind := in.Kind
	if kind == "" {
		kind = "service"
	}
	if _, ok := runpolicy.For(kind); !ok && kind != "volume" {
		return echo.NewHTTPError(http.StatusBadRequest, "kind must be service, cron or function")
	}
	t := &repo.Tile{
		StackID: s.ID, EnvironmentID: env.ID, ConnectorID: in.Connector,
		Name: in.Name, Kind: kind, SourceType: in.SourceType,
		ImageRef: in.Image, GitURL: strings.TrimSpace(in.GitURL), GitBranch: in.GitBranch,
		ContainerPort: in.Port, DockerfilePath: in.DockerfilePath, BuildContext: in.BuildContext,
		Env: mapToEnv(in.Env), Cron: in.Schedule, Command: in.Command,
		TimeoutMinutes: in.TimeoutMinutes,
	}
	// Every rule about the result — the slug, the cron expression, the source,
	// the connector's org, the kind-scoped keys — is the service's, and so is
	// the cron reload this path used to answer 500 for when it failed.
	if _, err := a.tiles.Create(ctx, t, nil, a.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusCreated, toAppOut(t))
}

func (a *API) getApp(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, toAppOut(t))
}

// notManaged refuses a managed database id on an /apps/ route. An instance is
// not an app: it has its own settings rules, its own redeploy predicate and a
// teardown that takes its slices, its shared network and the pool entry with
// it. These routes reached TileService directly, so `DELETE /apps/{db-id}`
// dropped the row and leaked all three, and `PATCH /apps/{db-id}` applied
// none of the instance rules and never recreated the container. The /dbs/
// routes are the ones that do this properly.
func notManaged(t *repo.Tile) error {
	if t.IsManaged() {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	return nil
}

func (a *API) patchApp(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := notManaged(t); err != nil {
		return err
	}
	ctx := c.Request().Context()
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	// The body is read twice on purpose. TileConf is value-typed, so a field
	// the caller left out and a field the caller set to zero decode to the
	// same thing: patching only `image` would blank the port, the limits and
	// every other numeric field. The raw key set is what separates absent
	// from cleared, and only keys actually present get written.
	//
	// This merge stays at the edge by design (D6): it is the shape of *this*
	// wire format, and the service is handed the finished row.
	var present map[string]json.RawMessage
	if err := json.Unmarshal(body, &present); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	var in appPatch
	if err := json.Unmarshal(body, &in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	has := func(keys ...string) bool {
		for _, k := range keys {
			if _, ok := present[k]; ok {
				return true
			}
		}
		return false
	}
	if has("image") {
		t.ImageRef = in.Image
		if in.Image != "" {
			t.SourceType = "image"
		}
	}
	if has("git_url") {
		t.GitURL = strings.TrimSpace(in.GitURL)
		if in.GitURL != "" {
			t.SourceType = "git"
		}
	}
	if has("branch") {
		t.GitBranch = in.Branch
	}
	if has("git_branch") {
		t.GitBranch = in.GitBranch
	}
	if has("connector") {
		t.ConnectorID = in.Connector
	}
	if has("port") {
		t.ContainerPort = in.Port
	}
	// limits and build are atomic pairs on the row but not in the request:
	// `--cpu` alone used to send memory_mb: 0 and zero the other half,
	// because assigning the whole struct treats an unsent field as a clear.
	// Merge per sub-key instead, so an absent half means unchanged.
	if sub := subKeys(present["build"]); in.Build != nil {
		if sub["dockerfile"] {
			t.DockerfilePath = in.Build.Dockerfile
		}
		if sub["context"] {
			t.BuildContext = in.Build.Context
		}
	}
	if sub := subKeys(present["limits"]); in.Limits != nil {
		if sub["cpu"] {
			t.CPULimit = in.Limits.CPU
		}
		if sub["memory_mb"] {
			t.MemLimitMB = in.Limits.MemoryMB
		}
	}
	if has("healthcheck") {
		t.HealthcheckCmd = in.Healthcheck
	}
	if has("healthcheck_interval") {
		t.HealthcheckIntervalS = in.HealthInterval
	}
	if has("healthcheck_timeout") {
		t.HealthcheckTimeoutS = in.HealthTimeout
	}
	if has("healthcheck_retries") {
		t.HealthcheckRetries = in.HealthRetries
	}
	if has("healthcheck_start_period") {
		t.HealthcheckStartPeriodS = in.HealthStartPeriod
	}
	if has("watch_paths") {
		t.WatchPaths = strings.Join(in.WatchPaths, "\n")
	}
	if has("build_args") {
		t.BuildArgs = in.BuildArgs
	}
	if has("published_ports") {
		t.PublishedPorts = in.PublishedPorts
	}
	if has("traefik_override") {
		t.TraefikOverride = in.TraefikOverride
	}
	if has("security_headers") {
		t.SecHeaders = in.SecurityHeaders
	}
	if has("basic_auth_user") {
		t.BasicAuthUser = in.BasicAuthUser
		if in.BasicAuthUser == "" {
			t.BasicAuthPassword = "" // no user, no credential left behind
		}
	}
	if has("basic_auth_password") {
		t.BasicAuthPassword = in.BasicAuthPassword
	}
	if has("command") {
		// Shared key: a cron's one-shot line (`sh -c`) or a service's CMD
		// override (argv), see runpolicy.AllowsCommand.
		t.Command = in.Command
	}
	if has("files") {
		t.Files = strings.Join(in.Files, "\n")
	}
	if has("storage") {
		t.Storage = strings.Join(in.Storage, "\n")
	}
	if has("depends_on") {
		// Sibling existence + cycles are validated where the whole env is in
		// view (config resolve / staged apply); the row accepts the shape.
		t.DependsOn = strings.Join(in.DependsOn, "\n")
	}
	if has("user") {
		t.User = in.User
	}
	if has("shm_size_mb") {
		t.ShmSizeMB = in.ShmSizeMB
	}
	if has("privileged") {
		t.Privileged = in.Privileged
	}
	if has("devices") {
		t.Devices = strings.Join(in.Devices, "\n")
	}
	if has("restart") {
		t.RestartPolicy = in.Restart
	}
	if has("run_on_deploy") {
		t.RunOnDeploy = in.RunOnDeploy
	}
	if has("schedule") {
		t.Cron = in.Schedule
	}
	if has("timeout_minutes") {
		t.TimeoutMinutes = in.TimeoutMinutes
	}
	if has("allow_overlap") {
		t.AllowOverlap = in.AllowOverlap
	}
	// Scale over the API and therefore over the CLI. The config file already
	// declares both and appPatch already parsed them; patchApp simply dropped
	// them, so `stackr tile scale` could not exist. An API that accepts a key
	// and ignores it is worse than one that refuses it, and worse again than
	// one that works.
	if has("replicas") {
		t.Replicas = in.Replicas
	}
	if has("node_group") {
		t.NodeGroup = in.NodeGroup
	}
	if has("update_policy") {
		t.UpdatePolicy = in.UpdatePolicy
	}
	if has("wait_for_ci") {
		t.WaitForCI = in.WaitForCI
	}
	// Everything above is the wire format. Every rule about the result — the
	// cron expression, the git URL, the connector's org, the mount grammars,
	// the kind-scoped keys, the replica guard — belongs to the service, and
	// so does whether this save stages, rewrites the route or redeploys.
	if _, err := a.tiles.Update(ctx, t, nil, a.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusOK, toAppOut(t))
}

// subKeys names which members of a nested JSON object the caller actually
// sent, so a partial object merges instead of replacing.
func subKeys(raw json.RawMessage) map[string]bool {
	out := map[string]bool{}
	if len(raw) == 0 {
		return out
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return out
	}
	for k := range m {
		out[k] = true
	}
	return out
}

func (a *API) deleteApp(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := notManaged(t); err != nil {
		return err
	}
	if _, err := a.tiles.Delete(c.Request().Context(), t, a.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (a *API) deployApp(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	// Managed, volume, cron and upper-env are all the service's refusals now.
	// The cron one is new here: the panel has always refused to deploy a cron
	// tile, and this path queued one, where a cron tile has no long-running
	// container for a deploy to replace.
	id, err := a.deploys.Trigger(c.Request().Context(), t, "api")
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusAccepted, deployAccepted{Deployment: id})
}

// runApp fires one immediate run of a cron or function tile. Detached: the
// run records into the tile's run history, same as the panel's "Run now".
func (a *API) runApp(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	run, err := a.life.RunNow(c.Request().Context(), t, a.actor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusAccepted, runAccepted{Started: true, Run: run.ID})
}

// stopRun ends a run in flight. The row closes as "stopped", not an error.
func (a *API) stopRun(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	stopped, err := a.life.StopRun(c.Request().Context(), t, c.Param("run"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusOK, runStopped{Stopped: stopped})
}

// --- deployments ---

func (a *API) listDeployments(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	ds, err := a.store.ListDeploymentsByTile(c.Request().Context(), t.ID, 20)
	if err != nil {
		return err
	}
	out := make([]deploymentOut, 0, len(ds))
	for i := range ds {
		out = append(out, toDeploymentOut(&ds[i]))
	}
	return c.JSON(http.StatusOK, out)
}

func toDeploymentOut(d *repo.Deployment) deploymentOut {
	return deploymentOut{ID: d.ID, AppID: d.TileID, Status: d.Status, Trigger: d.Trigger,
		CommitSHA: d.CommitSHA, ImageTag: d.ImageTag, Error: d.Error}
}

func (a *API) getDeployment(c echo.Context) error {
	d, err := a.loadDeployment(c, c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, toDeploymentOut(d))
}

// loadDeployment fetches a deployment and the tile it belongs to, which is
// where its tenancy comes from. It used to take the role the caller needed;
// the route's gate asks for that now (VerbDeploymentCancel on the cancel,
// VerbDeploymentRead on the reads).
func (a *API) loadDeployment(c echo.Context, id string) (*repo.Deployment, error) {
	d, err := a.store.GetDeployment(c.Request().Context(), id)
	if err != nil || d == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if _, err := a.tile(c, d.TileID); err != nil {
		return nil, err
	}
	return d, nil
}
