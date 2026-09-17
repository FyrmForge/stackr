package v1

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/runpolicy"
	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/storagetiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func (a *API) listApps(c echo.Context) error {
	proj := c.QueryParam("stack")
	env := c.QueryParam("env")
	apps, err := a.store.ListTiles(c.Request().Context())
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

// createApp, patchApp and deleteApp write straight through, no
// staging. On a stack whose canvas edits queue into the pending set, the same
// change made here lands immediately; see docs/features/api.md. Deliberate
// (docs/plan-parity.md item 5), not an oversight.
func (a *API) createApp(c echo.Context) error {
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
	var in appIn
	if err := c.Bind(&in); err != nil || in.Name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name required")
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
	src := in.SourceType
	if src == "" {
		if in.Image != "" {
			src = "image"
		} else {
			src = "git"
		}
	}
	branch := in.GitBranch
	if branch == "" {
		branch = "main"
	}
	kind := in.Kind
	if kind == "" {
		kind = "service"
	}
	if _, ok := runpolicy.For(kind); !ok {
		return echo.NewHTTPError(http.StatusBadRequest, "kind must be service, cron or function")
	}
	if kind == "function" && in.TimeoutMinutes == 0 {
		in.TimeoutMinutes = 30
	}
	// Cron parity with the web and config paths: an unvalidated schedule lands
	// in the DB and LoadSchedules silently skips it, so the job looks configured
	// and never runs. Crons build from git exactly like services now, the run
	// policy only forbids ingress.
	if pol, ok := runpolicy.For(kind); ok {
		if pol.RequiresSchedule {
			if err := jobs.ValidateCron(in.Schedule); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
			if in.TimeoutMinutes == 0 {
				in.TimeoutMinutes = 30
			}
		}
		if !pol.AllowsIngress && in.Port != 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "a "+kind+" has no endpoint; port does not apply")
		}
		if src == "image" && in.Image == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "image source requires an image")
		}
		if src == "git" {
			if in.GitURL == "" {
				return echo.NewHTTPError(http.StatusBadRequest, "git source requires a git_url")
			}
			if !repo.ValidGitURL(in.GitURL) {
				return echo.NewHTTPError(http.StatusBadRequest, "Use a GitHub URL: https://github.com/owner/repo or git@github.com:owner/repo")
			}
		}
	}
	dockerfile := in.DockerfilePath
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	buildContext := in.BuildContext
	if buildContext == "" {
		buildContext = "."
	}
	if err := a.checkConnector(ctx, s, in.Connector); err != nil {
		return err
	}
	now := time.Now().UTC()
	t := &repo.Tile{
		ID: uuid.New().String(), StackID: s.ID, EnvironmentID: env.ID, ConnectorID: in.Connector,
		Name: in.Name, Slug: slug, Kind: kind, SourceType: src,
		ImageRef: in.Image, GitURL: strings.TrimSpace(in.GitURL), GitBranch: branch, ContainerPort: in.Port,
		DockerfilePath: dockerfile, BuildContext: buildContext, Env: mapToEnv(in.Env),
		Cron: in.Schedule, Command: in.Command, TimeoutMinutes: in.TimeoutMinutes,
		WebhookToken: uuid.New().String(), Status: "idle", CreatedAt: now, UpdatedAt: now,
	}
	if err := a.store.CreateTile(ctx, t); err != nil {
		return err
	}
	if kind == "cron" && a.jobs != nil {
		// Without this the cron only starts running after the next restart.
		if err := a.jobs.LoadSchedules(ctx); err != nil {
			return err
		}
	}
	return c.JSON(http.StatusCreated, toAppOut(t))
}

func (a *API) getApp(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), false)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, toAppOut(t))
}

func (a *API) patchApp(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := a.rejectManaged(ctx, t.StackID); err != nil {
		return err
	}
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	// The body is read twice on purpose. TileConf is value-typed, so a field the
	// caller left out and a field the caller set to zero decode to the same
	// thing, patching only `image` would blank the port, the limits and every
	// other numeric field. The raw key set is what separates absent from
	// cleared, and only keys actually present get written.
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
		if in.GitURL != "" && !repo.ValidGitURL(in.GitURL) {
			return echo.NewHTTPError(http.StatusBadRequest, "Use a GitHub URL: https://github.com/owner/repo or git@github.com:owner/repo")
		}
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
		s, serr := a.store.GetStack(ctx, t.StackID)
		if serr != nil || s == nil {
			return echo.NewHTTPError(http.StatusNotFound, "not found")
		}
		if err := a.checkConnector(ctx, s, in.Connector); err != nil {
			return err
		}
		t.ConnectorID = in.Connector
	}
	if has("port") {
		if pol, ok := runpolicy.For(t.Kind); ok && !pol.AllowsIngress && in.Port != 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "a "+t.Kind+" has no endpoint; port does not apply")
		}
		t.ContainerPort = in.Port
	}
	if has("build") && in.Build != nil {
		t.DockerfilePath, t.BuildContext = in.Build.Dockerfile, in.Build.Context
	}
	if has("limits") && in.Limits != nil {
		t.CPULimit, t.MemLimitMB = in.Limits.CPU, in.Limits.MemoryMB
	}
	if has("healthcheck") {
		t.HealthcheckCmd = in.Healthcheck
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
	if t.BasicAuthUser != "" && t.BasicAuthPassword == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "basic_auth_user needs basic_auth_password")
	}
	if has("command") {
		// Shared key: a cron's one-shot line (`sh -c`) or a service's CMD
		// override (argv), see runpolicy.AllowsCommand.
		t.Command = in.Command
	}
	if has("files") {
		for _, l := range in.Files {
			if _, _, _, err := runtime.ParseFileMount(l); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
		}
		t.Files = strings.Join(in.Files, "\n")
	}
	if has("storage") {
		for _, l := range in.Storage {
			slug, _, _, _, err := storagetiles.ParseAttachment(l)
			if err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
			var st *repo.Storage
			if name := varref.OrgStorageRef(l); name != "" {
				s, serr := a.store.GetStack(ctx, t.StackID)
				if serr != nil || s == nil {
					return echo.NewHTTPError(http.StatusNotFound, "not found")
				}
				st, serr = a.store.GetOrgStorageBySlug(ctx, s.OrgID, name)
				if serr != nil || st == nil {
					return echo.NewHTTPError(http.StatusBadRequest, "org share "+name+" not found")
				}
			} else if st, err = a.store.GetStorageBySlug(ctx, slug); err != nil || st == nil {
				return echo.NewHTTPError(http.StatusBadRequest, "storage "+slug+" not found")
			}
			if err := storagetiles.ValidateAttach(st, t); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
		}
		t.Storage = strings.Join(in.Storage, "\n")
	}
	if has("depends_on") {
		for _, d := range in.DependsOn {
			if _, _, err := stackconf.ParseDep(d); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
		}
		// Sibling existence + cycles are validated where the whole env is in
		// view (config resolve / staged apply); the row accepts the shape.
		t.DependsOn = strings.Join(in.DependsOn, "\n")
	}
	if t.Kind == "service" {
		if has("user") {
			t.User = in.User
		}
		if has("shm_size_mb") {
			if in.ShmSizeMB < 0 {
				return echo.NewHTTPError(http.StatusBadRequest, "shm_size_mb must not be negative")
			}
			t.ShmSizeMB = in.ShmSizeMB
		}
		if has("privileged") {
			t.Privileged = in.Privileged
		}
		if has("devices") {
			for _, d := range in.Devices {
				if _, err := runtime.ParseDevice(d); err != nil {
					return echo.NewHTTPError(http.StatusBadRequest, err.Error())
				}
			}
			t.Devices = strings.Join(in.Devices, "\n")
		}
		if has("restart") {
			rp, err := runtime.NormalizeRestart(in.Restart)
			if err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
			t.RestartPolicy = rp
		}
	} else if has("user", "shm_size_mb", "privileged", "devices", "restart") {
		return echo.NewHTTPError(http.StatusBadRequest, "user, shm_size_mb, privileged, devices and restart apply to service tiles only")
	}
	if t.Kind == "service" {
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
		if t.HealthcheckIntervalS < 0 || t.HealthcheckTimeoutS < 0 || t.HealthcheckRetries < 0 || t.HealthcheckStartPeriodS < 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "healthcheck knobs must not be negative")
		}
	}
	if t.Kind == "function" {
		if has("run_on_deploy") {
			t.RunOnDeploy = in.RunOnDeploy
		}
		if has("timeout_minutes") {
			if in.TimeoutMinutes < 0 {
				return echo.NewHTTPError(http.StatusBadRequest, "timeout_minutes must not be negative")
			}
			t.TimeoutMinutes = in.TimeoutMinutes
		}
		if has("allow_overlap") {
			t.AllowOverlap = in.AllowOverlap
		}
		if has("schedule") {
			return echo.NewHTTPError(http.StatusBadRequest, "schedule applies to cron tiles only")
		}
	} else if has("run_on_deploy") {
		return echo.NewHTTPError(http.StatusBadRequest, "run_on_deploy applies to function tiles only")
	}
	if t.Kind == "cron" {
		if has("schedule") {
			// Validated here for the same reason createApp validates: an invalid
			// expression lands in the DB, LoadSchedules skips it, and the job
			// looks configured while never running.
			if err := jobs.ValidateCron(in.Schedule); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
			t.Cron = in.Schedule
		}
		if has("timeout_minutes") {
			if in.TimeoutMinutes < 0 {
				return echo.NewHTTPError(http.StatusBadRequest, "timeout_minutes must not be negative")
			}
			t.TimeoutMinutes = in.TimeoutMinutes
		}
		if has("allow_overlap") {
			t.AllowOverlap = in.AllowOverlap
		}
	} else if t.Kind != "function" && has("schedule", "timeout_minutes", "allow_overlap") {
		return echo.NewHTTPError(http.StatusBadRequest, "schedule, timeout_minutes and allow_overlap apply to cron and function tiles only")
	}
	if has("update_policy") {
		switch in.UpdatePolicy {
		case "", "off", "notify", "auto":
		default:
			return echo.NewHTTPError(http.StatusBadRequest, "update_policy must be off, notify or auto")
		}
		if (in.UpdatePolicy == "notify" || in.UpdatePolicy == "auto") && t.SourceType != "image" {
			return echo.NewHTTPError(http.StatusBadRequest, "update_policy watches an image source")
		}
		t.UpdatePolicy = in.UpdatePolicy
		if t.UpdatePolicy == "" {
			t.UpdatePolicy = "off"
		}
	}
	if has("wait_for_ci") {
		if in.WaitForCI && t.SourceType != "git" {
			return echo.NewHTTPError(http.StatusBadRequest, "wait_for_ci needs a git-built source")
		}
		t.WaitForCI = in.WaitForCI
	}
	// A runnable tile must leave the patch with a usable source: blanking a
	// cron's image used to slip through and strand a job that looks
	// configured but cannot run.
	if _, runnable := runpolicy.For(t.Kind); runnable {
		if t.SourceType == "image" && t.ImageRef == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "image source requires an image (set git_url to switch to a git build)")
		}
		if t.SourceType == "git" && t.GitURL == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "git source requires a git_url")
		}
	}
	t.UpdatedAt = time.Now().UTC()
	if err := a.store.UpdateTile(ctx, t); err != nil {
		return err
	}
	if t.Kind == "cron" && a.jobs != nil {
		// A changed schedule only takes effect on the next restart otherwise.
		_ = a.jobs.LoadSchedules(ctx)
	}
	return c.JSON(http.StatusOK, toAppOut(t))
}

func (a *API) deleteApp(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	if err := a.rejectManaged(c.Request().Context(), t.StackID); err != nil {
		return err
	}
	if err := a.teardownTile(c.Request().Context(), t); err != nil {
		return err
	}
	if t.Kind == "cron" && a.jobs != nil {
		_ = a.jobs.LoadSchedules(c.Request().Context())
	}
	return c.NoContent(http.StatusNoContent)
}

func (a *API) deployApp(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	if envnet.UpperEnv(c.Request().Context(), a.store, t) {
		return echo.NewHTTPError(http.StatusBadRequest, "this environment deploys by promote from the releases page")
	}
	id, err := a.engine.Enqueue(c.Request().Context(), t, "api")
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, deployAccepted{Deployment: id})
}

// runApp fires one immediate run of a cron or function tile. Detached: the
// run records into the tile's run history, same as the panel's "Run now".
func (a *API) runApp(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	if t.Kind != "cron" && t.Kind != "function" {
		return echo.NewHTTPError(http.StatusBadRequest, "run applies to cron and function tiles")
	}
	if a.jobs == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "job runner unavailable")
	}
	name := ""
	if k, _ := c.Get(ctxKey).(*repo.APIKey); k != nil {
		name = k.Name
	}
	run, err := a.jobs.StartApp(c.Request().Context(), t.ID, jobs.TriggerManualAPI, name)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, runAccepted{Started: true, Run: run.ID})
}

// stopRun ends a run in flight. The row closes as "stopped", not an error.
func (a *API) stopRun(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	if a.jobs == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "job runner unavailable")
	}
	run, err := a.store.GetCronRun(c.Request().Context(), c.Param("run"))
	if err != nil {
		return err
	}
	if run == nil || run.Ref != "app:"+t.ID {
		return echo.NewHTTPError(http.StatusNotFound, "run not found")
	}
	return c.JSON(http.StatusOK, runStopped{Stopped: a.jobs.Stop(c.Request().Context(), run.ID)})
}

// --- deployments ---

func (a *API) listDeployments(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), false)
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

// loadDeployment fetches a deployment and enforces access to its owning tile.
func (a *API) loadDeployment(c echo.Context, id string) (*repo.Deployment, error) {
	d, err := a.store.GetDeployment(c.Request().Context(), id)
	if err != nil || d == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if _, err := a.requireTile(c, d.TileID, false); err != nil {
		return nil, err
	}
	return d, nil
}
