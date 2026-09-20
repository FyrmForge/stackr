package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envutil"
	"github.com/FyrmForge/stackr/internal/stackrd/config/staging"
	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/githubapp"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/audit"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/placement"
	svcproxy "github.com/FyrmForge/stackr/internal/stackrd/service/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store  repo.Store
	clus   *cluster.Cluster // every docker call (docs/plans/35-cluster.md)
	px     *svcproxy.Service
	engine *deploy.Engine
	jobs   *jobs.Service
	// sched re-registers the cron and backup tables after a write that
	// changes or cascades their rows.
	sched    *scheduler.Service
	gh       *githubapp.Client
	notifier *notify.Notifier
	// life owns every runtime state change a person can ask for: stop,
	// restart, pause a schedule, run now, stop a run. tiles owns the row.
	life  *service.TileLifecycleService
	tiles *service.TileService
	// domains owns the hostnames a tile answers on.
	domains *service.DomainService
	slices  *service.SliceService
	vars    *service.VariableService
	envs    *service.EnvironmentService
	stacks  *service.StackService
	// telemetry resolves where a tile's logs and metrics come from.
	telemetry *service.TileTelemetryService
	// deploys owns the redeploy-if-running rule, which this file had two
	// copies of and storage.go two more.
	deploys *service.DeployService
	// gate answers whether a config-managed stack takes this edit, and
	// whether it stages. editGate was a second copy of it.
	gate *service.GateService
}

// NewHandler creates a new app handler.
// WithSlices gives the panel the slice service the API holds.
func (h *handler) WithSlices(sl *service.SliceService) *handler { h.slices = sl; return h }

// WithEnvironments gives the tile page the environment its tile sits in.
func (h *handler) WithEnvironments(e *service.EnvironmentService) *handler { h.envs = e; return h }

// WithStacks gives the tile page the stack it belongs to.
func (h *handler) WithStacks(s *service.StackService) *handler { h.stacks = s; return h }

// WithGate gives the panel the config-managed gate.
func (h *handler) WithGate(g *service.GateService) *handler { h.gate = g; return h }

// WithDeploys gives the panel the deploy service.
func (h *handler) WithDeploys(d *service.DeployService) *handler { h.deploys = d; return h }

// WithVariables gives the tile drawer the variable service.
func (h *handler) WithVariables(v *service.VariableService) *handler { h.vars = v; return h }

func NewHandler(store repo.Store, clus *cluster.Cluster, px *svcproxy.Service, engine *deploy.Engine, jobsSvc *jobs.Service, gh *githubapp.Client, notifier *notify.Notifier, life *service.TileLifecycleService, tiles *service.TileService, telemetry *service.TileTelemetryService, domains *service.DomainService) *handler {
	return &handler{store: store, clus: clus, px: px, engine: engine, jobs: jobsSvc, gh: gh, notifier: notifier, life: life, tiles: tiles, telemetry: telemetry, domains: domains}
}

// GET /apps/:id/connectors, <option>s for the git connector select.
func (h *handler) Connectors(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var conns []repo.Connector
	if stack, err := h.stacks.Get(ctx, a.StackID); err == nil {
		all, _ := h.store.ListConnectorsByOrg(ctx, stack.OrgID)
		for _, cn := range all {
			if cn.Provider == "github" && githubapp.ParseConfig(cn.Config).Connected() {
				conns = append(conns, cn)
			}
		}
	}
	return respond.HTML(c, http.StatusOK, connectorOptions(conns, a.ConnectorID))
}

// GET /apps/:id/repos, datalist <option>s of repos visible to the tile's
// connector (explicit or org fallback).
func (h *handler) Repos(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	cn := h.gh.ConnectorForTile(ctx, a)
	if cn == nil {
		return respond.HTML(c, http.StatusOK, repoOptions(nil))
	}
	repos, err := h.gh.ListRepos(ctx, cn)
	if err != nil {
		c.Logger().Warnf("github repo list (%s): %v", cn.Name, err)
	}
	return respond.HTML(c, http.StatusOK, repoOptions(repos))
}

// GET /apps/:id/logs/stream, SSE live-follow of the tile's container logs
// ("O"/"E" marked lines with docker timestamps; see components.LogView).
func (h *handler) LogsStream(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/event-stream")
	res.Header().Set("Cache-Control", "no-cache")
	res.WriteHeader(http.StatusOK)
	// Where the lines come from is a rule about how the tile is deployed,
	// and it lives in the telemetry service so this handler and the API's
	// cannot resolve it differently.
	src := h.telemetry.Logs(ctx, a)
	if !src.Found() {
		_, _ = fmt.Fprint(res, "data: O - no running container. Deploy first (cron tiles only log per run)\n\n")
		res.Flush()
		return nil
	}
	var (
		ch   <-chan string
		stop func()
	)
	if src.Service != "" {
		ch, stop, err = h.clus.StreamServiceLogsMarked(ctx, src.Service, 300)
	} else {
		ch, stop, err = h.clus.StreamLogsMarked(ctx, h.clus.Self(ctx), src.Container, 300)
	}
	if err != nil {
		return err
	}
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case line, ok := <-ch:
			if !ok {
				return nil
			}
			_, _ = fmt.Fprintf(res, "data: %s\n\n", line)
			res.Flush()
		}
	}
}

// GET /apps/:id/metrics?range=1h|6h|24h, the charts fragment (page + panel).
func (h *handler) Metrics(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	key, dur := components.MetricRange(c.QueryParam("range"))
	cpu, mem, rx, tx := h.telemetry.Points(c.Request().Context(), service.TileRef(a.ID), dur)
	return respond.HTML(c, http.StatusOK, metricsFrag(a, key, cpu, mem, rx, tx))
}

// POST /apps/:id/run, run a cron-kind app immediately (in background).
func (h *handler) RunNow(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	// No flash: the header says "running" and the Runs tab has the row.
	if _, err := h.life.RunNow(c.Request().Context(), a, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	return h.headerDone(c, a)
}

// POST /apps/:id/runs/:run/stop, stop a run in flight. The row closes as
// "stopped" rather than an error; see jobs.Service.Stop for what that kills.
func (h *handler) StopRun(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	stopped, err := h.life.StopRun(c.Request().Context(), a, c.Param("run"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if !stopped {
		middleware.SetFlash(c, "That run already finished.", middleware.FlashInfo)
	}
	return h.headerDone(c, a)
}

// runWindow is how many runs the Runs tab lists and the Logs tab replays.
const runWindow = 20

func (h *handler) RunsLogsStream(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	runs, err := h.store.ListCronRuns(ctx, service.TileRef(a.ID), runWindow)
	if err != nil {
		return err
	}
	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/event-stream")
	res.Header().Set("Cache-Control", "no-cache")
	res.WriteHeader(http.StatusOK)

	// Newest first out of the store; replay the other way round so the stream
	// reads forwards in time.
	var open *repo.CronRun
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		if r.Running() {
			open = &runs[i] // output only lands on the row when it ends
			continue
		}
		replayRun(res, r)
	}
	res.Flush()
	if open == nil {
		return sseDone(res)
	}
	// By service, not by container: the run's task can be on a worker, where
	// the manager's container list is empty and the stream ended with the
	// run still going.
	name := h.jobs.RunService(ctx, a, open.ID)
	if name == "" {
		return sseDone(res)
	}
	ch, stop, err := h.clus.StreamServiceLogsMarked(ctx, name, 300)
	if err != nil {
		return sseDone(res)
	}
	defer stop()
	tag := shortRun(open.ID)
	for {
		select {
		case <-ctx.Done():
			return nil
		case line, ok := <-ch:
			if !ok {
				return sseDone(res)
			}
			_, _ = fmt.Fprintf(res, "data: %s|%s\n\n", tag, line)
			res.Flush()
		}
	}
}

// replayRun writes one run's stored output as SSE lines in the format the
// viewer parses: "<short id>|O <ts> <msg>". Stored output has no per-line
// time, so every line carries the run's start. A run that stored nothing
// writes nothing, a bare "O <ts> " renders as an empty row.
func replayRun(w io.Writer, r repo.CronRun) {
	if r.Output == "" {
		return
	}
	tag, ts := shortRun(r.ID), r.StartedAt.Format(time.RFC3339)
	for _, line := range strings.Split(strings.TrimRight(r.Output, "\n"), "\n") {
		_, _ = fmt.Fprintf(w, "data: %s|O %s %s\n\n", tag, ts, line)
	}
}

// sseDone ends a stream for good: a plain close makes EventSource reconnect
// and replay, so the viewer only stops when it sees this event.
func sseDone(res *echo.Response) error {
	_, _ = fmt.Fprint(res, "event: done\ndata: end\n\n")
	res.Flush()
	return nil
}

// shortRun is the run id as it appears in the log tag and the Runs tab.
func shortRun(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// POST /apps/:id/stop, scale the tile's service to zero, keeping the spec
// (and its deployment record) around so Deploy/Restart brings the same thing
// back. Stopping the task's container instead would just make swarm start
// another one.
// POST /apps/:id/stop. No flash: SetFlash only surfaces on the *next*
// request, and the header re-render already shows the new status badge.
func (h *handler) Stop(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	if err := h.life.Stop(c.Request().Context(), a); err != nil {
		return stackrmw.HTTP(err)
	}
	return h.headerDone(c, a)
}

// POST /apps/:id/restart, bounce the tile. Deliberately not a redeploy: no
// rebuild, no new image, the same spec re-rolled.
func (h *handler) Restart(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	if err := h.life.Restart(c.Request().Context(), a); err != nil {
		return stackrmw.HTTP(err)
	}
	return h.headerDone(c, a)
}

// POST /apps/:id/cron/toggle, pause/resume a cron-kind app's schedule.
func (h *handler) ToggleCron(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	status, err := h.life.ToggleCron(c.Request().Context(), a)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if status == "paused" {
		middleware.SetFlash(c, "Schedule paused.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "Schedule resumed.", middleware.FlashSuccess)
	}
	return h.headerDone(c, a)
}

// GET /apps/:id/branches, datalist <option>s of the repo's remote branches.
func (h *handler) Branches(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, branchOptions(h.engine.ListBranches(c.Request().Context(), a)))
}

func (h *handler) load(c echo.Context) (*repo.Tile, error) {
	a, err := h.tiles.Get(c.Request().Context(), c.Param("id"))
	if err != nil {
		return nil, stackrmw.HTTP(err)
	}
	// The drawer has no breadcrumb, so its header says where the tile
	// lives. Resolved here because every panel path comes through load().
	components.StashTileLocation(c, h.store, a)
	components.StashUpperEnv(c, h.store, a)
	return a, nil
}

// tabData loads just what the requested tab renders. Shared by the full page,
// the drawer, and the tab-content swaps so all three stay in lockstep.
type tabData struct {
	deployments      []repo.Deployment
	domains          []repo.Domain
	cpu, mem, rx, tx []components.TimePoint
	runs             []repo.CronRun
	openRun          *repo.CronRun // cron/function only: the run in flight, if any
	vol              volumeView    // volume-tile-only: attach targets + measured size
	httpLog          []proxy.AccessEntry
	configMode       string // "" = UI-managed; else the stack's ui_edits mode (block | stage)
	// node is where this tile's data and task live, named and with its
	// current state. A tile pinned to a machine that is off reported nothing
	// but "stopped", which sends the operator to the tile's logs when the
	// answer is one layer down.
	node placementView
}

// placementView is the tile's node as the overview shows it: the machine's
// name and what state it is in. Empty Name means the tile is not pinned
// anywhere, which is the normal case for a stateless tile on one machine.
//
// Status, not a Down bool: a server is pending, ready, draining or down
// (repo.Server.Status), and collapsing three of those into one flag made the
// overview tell an operator their node was "not answering" thirty seconds
// after they drained it themselves. The cause it
// names is the fix it implies, so naming the wrong one sends them to the
// network when the answer is one click away.
type placementView struct {
	Name   string
	Status string
}

// Down is the "this tile cannot run" case, whatever the reason.
func (p placementView) Down() bool { return p.Status != "" && p.Status != "ready" }

// placementOf names the node a tile's data is on and says whether it is up.
//
// Reads through placement.NodeOf rather than the tile's own column: a volume
// tile has no home node of its own, its node is the one its attached tile
// runs on.
func (h *handler) placementOf(ctx context.Context, a *repo.Tile) placementView {
	nodeID := placement.NodeOf(ctx, h.store, a)
	if nodeID == "" {
		return placementView{}
	}
	sv, err := h.store.GetServerByNodeID(ctx, nodeID)
	if err != nil || sv == nil {
		// Pinned to a node with no row: it left the swarm. Say the id, which
		// is all there is, rather than nothing.
		return placementView{Name: nodeID, Status: "gone"}
	}
	name := sv.Hostname
	if name == "" {
		name = sv.Name
	}
	return placementView{Name: name, Status: sv.Status}
}

// defaultTab is what opens when no tab is named: overview for services, runs
// for crons, what a cron is opened to look at is what it did, and an
// overview with no domains or ports would be empty anyway.
func defaultTab(a *repo.Tile) string {
	if a.Kind == "cron" || a.Kind == "function" {
		return "runs"
	}
	return "overview"
}

// runsTile: the kinds whose history is runs rather than a long-lived
// container.
func runsTile(a *repo.Tile) bool { return a.Kind == "cron" || a.Kind == "function" }

func (h *handler) loadTab(c echo.Context, a *repo.Tile, tab string) (tabData, error) {
	ctx := c.Request().Context()
	var d tabData
	var err error
	if tab == "" {
		tab = defaultTab(a)
	}
	// Every tab, not just Runs: the header carries the run state, and it is
	// rendered above whichever tab is open.
	if runsTile(a) {
		d.openRun, _ = h.store.OpenCronRun(ctx, service.TileRef(a.ID))
	}
	switch tab {
	case "overview":
		if a.IsVolume() {
			d.vol = h.volumeView(ctx, a)
			break
		}
		if d.domains, err = h.store.ListDomainsByTile(ctx, a.ID); err != nil {
			return d, err
		}
		if d.deployments, err = h.store.ListDeploymentsByTile(ctx, a.ID, 1); err != nil {
			return d, err
		}
		d.node = h.placementOf(ctx, a)
	case "logs":
		// live view, content arrives over SSE (LogsStream), nothing to preload.
		// A cron has no container to follow, so its Logs tab picks a run.
		if runsTile(a) {
			d.runs, _ = h.store.ListCronRuns(ctx, service.TileRef(a.ID), runWindow)
		}
	case "runs":
		d.runs, _ = h.store.ListCronRuns(ctx, service.TileRef(a.ID), runWindow)
	case "http":
		d.httpLog = h.px.AccessLog(a.ID, 100)
	case "settings":
		if a.IsVolume() {
			d.vol = h.volumeView(ctx, a)
			break
		}
		if d.domains, err = h.store.ListDomainsByTile(ctx, a.ID); err != nil {
			return d, err
		}
		d.configMode = h.configMode(ctx, a.StackID)
		// Placement needs it too, not just the overview: without this the
		// Settings tab printed the raw swarm node id where every other screen
		// says the hostname.
		d.node = h.placementOf(ctx, a)
	case "metrics":
		_, dur := components.MetricRange(c.QueryParam("range"))
		d.cpu, d.mem, d.rx, d.tx = h.telemetry.Points(ctx, service.TileRef(a.ID), dur)
	case "variables":
		// app already carries env/build_args
	default: // deployments, for every kind that has them
		if d.deployments, err = h.store.ListDeploymentsByTile(ctx, a.ID, 10); err != nil {
			return d, err
		}
	}
	return d, nil
}

// GET /apps/:id?tab=..., full page: the same tabbed panel as the drawer.
func (h *handler) Detail(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	tab := c.QueryParam("tab")
	if tab == "" {
		tab = defaultTab(a)
	}
	d, err := h.loadTab(c, a, tab)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, appPage(c, a, tab, h.crumb(c, a), d.openRun, d.deployments, d.domains, d.cpu, d.mem, d.rx, d.tx, d.runs, d.vol, d.httpLog, d.configMode, d.node))
}

// crumb builds the breadcrumb trail for the full-page view: stack and env
// names with links back to the canvas.
func (h *handler) crumb(c echo.Context, a *repo.Tile) breadcrumb {
	ctx := c.Request().Context()
	b := breadcrumb{StackHref: "/projects/" + a.StackID, StackName: "stack"}
	stack, _ := h.stacks.Get(ctx, a.StackID)
	env, _ := h.envs.Get(ctx, a.EnvironmentID)
	if stack == nil || env == nil {
		return b
	}
	b.StackName = stack.Name
	// Fallback keeps the crumb clickable-ish for an orphaned stack; the org
	// id is at least honest where a magic slug would not be.
	orgSlug := stack.OrgID
	if o, _ := h.store.GetOrg(ctx, stack.OrgID); o != nil && o.Slug != "" {
		orgSlug = o.Slug
	}
	b.EnvName = env.Name
	b.EnvHref = "/" + orgSlug + "/" + stack.Slug + "/" + env.Slug
	return b
}

// GET /apps/:id/panel, the full right-drawer for this app (default tab).
func (h *handler) Panel(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	d, err := h.loadTab(c, a, "")
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, appPanel(c, a, defaultTab(a), d.openRun, d.deployments, d.domains, d.cpu, d.mem, d.rx, d.tx, d.runs, d.vol, d.httpLog, d.configMode, d.node, true))
}

// GET /apps/:id/panel/content?tab=..., swaps just the tabbed content region.
func (h *handler) PanelContent(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	tab := c.QueryParam("tab")
	if tab == "" {
		tab = defaultTab(a)
	}
	d, err := h.loadTab(c, a, tab)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, components.WithFlash(c, panelContent(c, a, tab, d.deployments, d.domains, d.cpu, d.mem, d.rx, d.tx, d.runs, d.vol, d.httpLog, d.configMode, d.node)))
}

// headerDone answers a status action (run, pause, resume): the header
// re-renders out of band and the tab body is left untouched, so the user keeps
// the tab they were reading.
func (h *handler) headerDone(c echo.Context, a *repo.Tile) error {
	if c.Request().Header.Get("HX-Request") != "true" {
		return respond.Redirect(c, "/apps/"+a.ID)
	}
	return respond.HTML(c, http.StatusOK, panelHeaderOOB(c, a, h.openRun(c, a), true))
}

// openRun is the run in flight for a cron or function tile, nil otherwise.
func (h *handler) openRun(c echo.Context, a *repo.Tile) *repo.CronRun {
	if !runsTile(a) {
		return nil
	}
	r, _ := h.store.OpenCronRun(c.Request().Context(), service.TileRef(a.ID))
	return r
}

// GET /apps/:id/panel/header, the header on its own, so it can refresh
// itself on a project event instead of going stale under an open tab.
func (h *handler) PanelHeader(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, panelHeader(c, a, h.openRun(c, a), c.QueryParam("drawer") != ""))
}

// envServices lists the plain services in the tile's environment, the
// attach-target choices for a volume panel.
func (h *handler) envServices(ctx context.Context, a *repo.Tile) []repo.Tile {
	tiles, err := h.tiles.ListForEnv(ctx, a.EnvironmentID)
	if err != nil {
		return nil
	}
	var out []repo.Tile
	for i := range tiles {
		if tiles[i].Kind == "service" && !tiles[i].IsManaged() {
			out = append(out, tiles[i])
		}
	}
	return out
}

// volumeView gathers what a volume tile's panel needs beyond the tile itself:
// its attach-target choices. The size is its own request, Size.
func (h *handler) volumeView(ctx context.Context, a *repo.Tile) volumeView {
	return volumeView{Services: h.envServices(ctx, a)}
}

// GET /apps/:id/size, the volume's size cell on the overview.
//
// The size costs a docker DiskUsage scan, which is why it is measured here,
// after the panel is up, and not on the canvas. Painting a warning badge on
// every volume card would mean that scan on every graph render, for a limit
// nothing enforces anyway.
func (h *handler) Size(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	size := int64(-1)
	if info, err := h.clus.InspectVolume(c.Request().Context(), a.DockerVolume()); err == nil {
		size = info.SizeBytes
	}
	return respond.HTML(c, http.StatusOK, volumeSize(a, size))
}

// Attach points a volume at a service (or detaches it) and redeploys the
// affected service(s) so the mounts take effect.
// POST /apps/:id/attach
func (h *handler) Attach(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	if !a.IsVolume() {
		return echo.NewHTTPError(http.StatusBadRequest, "not a volume tile")
	}
	ctx := c.Request().Context()
	oldTarget := a.AttachedTileID
	targetID := c.FormValue("attach_tile_id")
	path := strings.TrimSpace(c.FormValue("mount_path"))
	// The attachment and mount path are config-modeled; the expected size is
	// not. So a config-managed stack rejects only a real attach/path change and
	// leaves the size editable, the same split SetPort makes for a database's
	// published port. Checked before anything is assigned to the tile.
	//
	// The gate runs either way, because it also decides whether this save
	// stages: a size-only edit on a block stack has to write straight through,
	// not drop a pending patch carrying the attach and path it never touched.
	// Only a real attach/path change propagates its refusal.
	stage, gateErr := h.editGate(ctx, a.StackID)
	if targetID != a.AttachedTileID || path != a.MountPath {
		if gateErr != nil {
			return gateErr
		}
	}
	targetSlug := ""
	if targetID != "" {
		target, err := h.tiles.Get(ctx, targetID)
		if err != nil || target.EnvironmentID != a.EnvironmentID || target.Kind != "service" || target.IsManaged() {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid attach target")
		}
		if !strings.HasPrefix(path, "/") {
			return echo.NewHTTPError(http.StatusBadRequest, "mount path must be absolute (e.g. /data)")
		}
		targetSlug = target.Slug
	}
	a.AttachedTileID = targetID
	a.MountPath = path
	// Warn-only, so a bad number is not worth rejecting the whole save over:
	// anything unparseable or negative means "no ceiling".
	if n, err := strconv.Atoi(strings.TrimSpace(c.FormValue("max_size_mb"))); err == nil && n >= 0 {
		a.MaxSizeMB = n
	} else {
		a.MaxSizeMB = 0
	}

	// Stage the attach/detach (config references the target by slug).
	if stage {
		// The expected size is not config-modeled and deploys nothing, so it
		// saves straight through rather than waiting behind an apply. Written
		// from the stored row so the staged attach/path don't ride along.
		if stored, err := h.tiles.Get(ctx, a.ID); err == nil {
			stored.MaxSizeMB = a.MaxSizeMB
			if err := h.store.UpdateTile(ctx, stored); err != nil {
				return err
			}
		}
		patch := map[string]any{"attach": targetSlug, "path": path}
		if err := h.stage(c, a, "volume", staging.OpUpdate, patch); err != nil {
			return err
		}
		middleware.SetFlash(c, "Volume change staged. Review & apply on the canvas to deploy.", middleware.FlashSuccess)
		return h.panelDone(c, a, "settings")
	}

	if err := h.store.UpdateTile(ctx, a); err != nil {
		return err
	}
	// Redeploy whichever services gained or lost the mount.
	for _, id := range []string{oldTarget, targetID} {
		if id == "" || (id == oldTarget && oldTarget == targetID) {
			continue
		}
		// Only if it is up: a mount exists on the next container either way,
		// and queueing a deploy for a stopped service would start it behind
		// the user's back.
		h.deploys.RedeployIfRunning(ctx, id, "volume")
	}
	if targetID == "" {
		middleware.SetFlash(c, "Volume detached.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "Volume attached. The service is redeploying with the new mount.", middleware.FlashSuccess)
	}
	return h.panelDone(c, a, "settings")
}

// GET /apps/:id/provisions, the shared-databases fragment on settings.
func (h *handler) Provisions(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	return h.renderProvisions(c, a)
}

func (h *handler) renderProvisions(c echo.Context, a *repo.Tile) error {
	ctx := c.Request().Context()
	ps, err := h.store.ListProvisionsByConsumer(ctx, a.ID)
	if err != nil {
		return err
	}
	rows := make([]provisionRow, 0, len(ps))
	for _, p := range ps {
		name, ref := "?", ""
		if t, _ := h.tiles.Get(ctx, p.InstanceTileID); t != nil {
			name = t.Name
			ref = managedtiles.Ref(t, &p, managedtiles.DefaultOutput(t.Engine))
		}
		rows = append(rows, provisionRow{P: p, InstanceName: name, Ref: ref})
	}
	instances, err := managedtiles.EligibleInstances(ctx, h.store, a)
	if err != nil {
		return err
	}
	attachable, err := managedtiles.AttachableDBs(ctx, h.store, a)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, provisionsFrag(c, a, rows, instances, attachable))
}

// wireProvision optionally injects the provision's url secret into the
// consumer's env under varName and redeploys (the redeploy joins the shared
// net). Returns the user-facing flash. Blank var = publish-only.
// infraAddress is the instance's colon-path, for copy that tells someone what
// to put in their config file. Best-effort: a lookup failure yields the slug,
// which is wrong but obviously so, rather than a confidently wrong address.
func (h *handler) infraAddress(ctx context.Context, instance *repo.Tile) string {
	sc, err := envnet.Resolve(ctx, h.store, instance)
	if err != nil {
		return instance.Slug
	}
	st, err := h.stacks.Get(ctx, instance.StackID)
	if err != nil {
		return instance.Slug
	}
	org, err := h.store.GetOrg(ctx, st.OrgID)
	if err != nil || org == nil {
		return instance.Slug
	}
	return managedtiles.InfraPath(instance.ScopeKind, org.Slug, sc.StackSlug, sc.EnvSlug, instance.Slug)
}

func (h *handler) wireProvision(ctx context.Context, a *repo.Tile, instance *repo.Tile, p *repo.Provision, varName, verb string) (string, error) {
	// On a config-managed stack the file owns tile.Env: injecting here would be
	// stripped by the next apply, leaving the app without its DB URL. The file
	// can declare the whole relationship with uses:, so point at that rather
	// than at a hand-written reference the next plan would show as drift.
	if !h.uiManaged(ctx, a.StackID) {
		if varName == "" {
			varName = managedtiles.DefaultOutput(instance.Engine)
		}
		return "Database " + verb + ". Declare it in your config file so a plan can see it:\n" +
			"uses:\n  - infra: " + h.infraAddress(ctx, instance) + "\n    name: " + p.DBName +
			"\n    var: " + varName, nil
	}
	used, err := h.slices.Wire(ctx, instance, a, p, varName)
	if err != nil {
		return "", err
	}
	// An s3 slice injects its whole output set and ignores the asked-for name
	// (one endpoint is not enough to reach a bucket with), so it reports none.
	if used == "" {
		if managedtiles.Engines[instance.Engine].AutoInjectAll {
			return "Database " + verb + ". Its connection details were set on " + a.Name +
				", which is redeploying.", nil
		}
		ref := managedtiles.Ref(instance, p, managedtiles.DefaultOutput(instance.Engine))
		return "Database " + verb + ". Add " + ref + " to this tile's variables; it applies on the next deploy.", nil
	}
	msg := "Database " + verb + ". " + used + " set and " + a.Name + " is redeploying."
	if used != service.EnvVarName(varName) {
		msg = "Database " + verb + ". " + service.EnvVarName(varName) +
			" was already used, so " + used + " was set instead; " + a.Name + " is redeploying."
	}
	return msg, nil
}

// POST /apps/:id/provision, create a logical db in a shared instance.
func (h *handler) Provision(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	if err := h.rejectManaged(c.Request().Context(), a.StackID); err != nil {
		return err
	}
	if a.IsManaged() || a.IsVolume() {
		return echo.NewHTTPError(http.StatusBadRequest, "only services and crons can consume provisions")
	}
	ctx := c.Request().Context()
	instance, err := h.tiles.Get(ctx, c.FormValue("instance_id"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	p, err := h.slices.Provision(ctx, instance, a, "", false)
	if err != nil {
		middleware.SetFlash(c, "Provisioning failed: "+err.Error(), middleware.FlashError)
		return h.renderProvisions(c, a)
	}
	msg, err := h.wireProvision(ctx, a, instance, p, c.FormValue("env_var"), "provisioned")
	if err != nil {
		return err
	}
	middleware.SetFlash(c, msg, middleware.FlashSuccess)
	return h.renderProvisions(c, a)
}

// POST /apps/:id/provisions/attach, share an existing logical db (same env).
func (h *handler) AttachProvision(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	if err := h.rejectManaged(c.Request().Context(), a.StackID); err != nil {
		return err
	}
	if a.IsManaged() || a.IsVolume() {
		return echo.NewHTTPError(http.StatusBadRequest, "only services and crons can consume provisions")
	}
	ctx := c.Request().Context()
	src, err := h.store.GetProvision(ctx, c.FormValue("provision_id"))
	if err != nil {
		return err
	}
	p, instance, err := h.slices.Attach(ctx, src, a)
	if err != nil {
		middleware.SetFlash(c, "Attach failed: "+err.Error(), middleware.FlashError)
		return h.renderProvisions(c, a)
	}
	msg, err := h.wireProvision(ctx, a, instance, p, c.FormValue("env_var"), "attached")
	if err != nil {
		return err
	}
	middleware.SetFlash(c, msg, middleware.FlashSuccess)
	return h.renderProvisions(c, a)
}

// POST /apps/:id/provisions/:pid/detach, unlink, keep the data (orphaned).
func (h *handler) DetachProvision(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	if err := h.rejectManaged(c.Request().Context(), a.StackID); err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := h.slices.Detach(ctx, a, c.Param("pid")); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Detached. The data was kept. Drop it from the instance's panel.", middleware.FlashSuccess)
	return h.renderProvisions(c, a)
}

// panelDone finishes a panel-form save: htmx requests get the re-rendered
// panel content in place (both the drawer and the full page host
// #panel-content), plain requests fall back to the redirect.
func (h *handler) panelDone(c echo.Context, a *repo.Tile, tab string) error {
	if c.Request().Header.Get("HX-Request") != "true" {
		return respond.Redirect(c, "/apps/"+a.ID+"?tab="+tab)
	}
	d, err := h.loadTab(c, a, tab)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, panelContent(c, a, tab, d.deployments, d.domains, d.cpu, d.mem, d.rx, d.tx, d.runs, d.vol, d.httpLog, d.configMode, d.node))
}

// GET /apps/:id/vars, the Variables tab body. Loaded separately from the
// panel so the rows and the reference catalogue are fetched only when the tab
// is actually opened, and so every mutation below can re-render just this.
func (h *handler) Vars(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	vars, err := h.vars.List(ctx, service.TileVars(a.ID))
	if err != nil {
		return err
	}
	sources, err := varref.Catalogue(ctx, h.store, a.ID)
	if err != nil {
		return err
	}
	// newest 50, no paging, add paging when someone asks to scroll back.
	events, err := h.store.ListAuditEvents(ctx, repo.OwnerTile, a.ID, 50)
	if err != nil {
		return err
	}
	// A staged removal leaves the row alone, the value is live until the
	// pending set is applied, so the list marks it instead of hiding it.
	pending := map[string]bool{}
	if desired, derr := h.currentDesiredEnv(ctx, a); derr == nil {
		for name := range envMapFromBlob(a.Env) {
			if _, kept := desired[name]; !kept {
				pending[name] = true
			}
		}
	}
	return respond.HTML(c, http.StatusOK, varsFrag(c, a, vars, sources, events, h.configMode(ctx, a.StackID), pending))
}

// VarValue hands one tile variable's plaintext to the Reveal button and is the
// audit hook for it, tile secrets no longer ride the tab markup masked.
// GET /apps/:id/vars/value?name=X
func (h *handler) VarValue(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	// A reveal hands out plaintext, so it takes write rights in the tile's
	// own org. That is the route's verb now (VerbVariableRead, write level),
	// which is why the stack this used to load for its own check is gone.
	vars, err := h.vars.List(c.Request().Context(), service.TileVars(a.ID))
	if err != nil {
		return err
	}
	return stackrmw.AuditServeValue(c, h.store, vars, repo.OwnerTile, a.ID, audit.Reveal)
}

// POST /apps/:id/vars/secret, add or replace one secret variable. Secrets are
// written as rows only: the raw env editor is rendered in plain text and its
// contents are diffed into config plans.
func (h *handler) SaveSecretVar(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	// The name rule is the service's now. This form used to accept any
	// non-empty string, so a tile secret could be given a name the panel
	// refused one scope up and that no shell could read.
	if err := h.vars.Set(c.Request().Context(), service.TileVars(a.ID), []service.VarWrite{{
		Name: strings.TrimSpace(c.FormValue("name")), Value: components.VarValue(c), Secret: true,
	}}, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Secret saved. It applies on the next deploy.", middleware.FlashSuccess)
	return h.Vars(c)
}

// POST /apps/:id/vars/delete, remove one variable row.
func (h *handler) DeleteVar(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	name := c.FormValue("name")
	// A name that lives in the tile's env blob is not really a row: every tile
	// write mirrors the blob back into variables (projectEnvVars), so deleting
	// the row alone was undone by the next save. Editing the blob is the
	// delete, and the blob is a field the config file owns, so it gates and
	// then stages, exactly like SaveEnv.
	env, err := h.currentDesiredEnv(ctx, a)
	if err != nil {
		return err
	}
	// A secret never came from the file, so it deletes directly even when a
	// name of the same spelling also sits in the blob, which is exactly what
	// the Variables list still offers under ui_edits: block.
	secret := false
	if vars, verr := h.vars.List(ctx, service.TileVars(a.ID)); verr == nil {
		for _, v := range vars {
			if v.Name == name && v.Secret {
				secret = true
			}
		}
	}
	if _, inBlob := env[name]; inBlob && !secret {
		if _, err := h.editGate(ctx, a.StackID); err != nil {
			return err
		}
		delete(env, name)
		if err := h.stage(c, a, "env vars", staging.OpUpdate,
			map[string]any{"env": env, "build_args": a.BuildArgs}); err != nil {
			return err
		}
		// No audit event: nothing has been deleted yet, and the staged entry
		// already carries who asked. Recorded when the pending set applies,
		// the same way SaveEnv leaves its staged edit unaudited.
		middleware.SetFlash(c, name+" removal staged. Review & apply on the canvas.", middleware.FlashSuccess)
		return h.Vars(c)
	}
	if err := h.vars.Unset(ctx, service.TileVars(a.ID), []string{name}, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Variable deleted. It stops being set on the next deploy.", middleware.FlashSuccess)
	return h.Vars(c)
}

// POST /apps/:id/env, save only env + build args (from the Variables tab).
func (h *handler) SaveEnv(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	newEnv := c.FormValue("env")

	// Structural edits (env + build args) go into the per-env pending set,
	// reviewed and applied as one transaction. A config-managed stack only
	// gets here when it declares ui_edits: stage; otherwise editGate refuses.
	stage, err := h.editGate(ctx, a.StackID)
	if err != nil {
		return err
	}
	if stage {
		newBuildArgs := c.FormValue("build_args")
		if newEnv == a.Env && newBuildArgs == a.BuildArgs {
			middleware.SetFlash(c, "No changes to variables.", middleware.FlashSuccess)
			return h.panelDone(c, a, "variables")
		}
		// Hold the edits in memory (not committed) so the panel re-renders them;
		// the staged buffer, not the tile row, carries the pending values.
		a.Env, a.BuildArgs = newEnv, newBuildArgs
		patch := map[string]any{"env": envMapFromBlob(newEnv), "build_args": newBuildArgs}
		if err := h.stage(c, a, "env vars", staging.OpUpdate, patch); err != nil {
			return err
		}
		middleware.SetFlash(c, "Variables staged. Review & apply on the canvas to deploy.", middleware.FlashSuccess)
		return h.panelDone(c, a, "variables")
	}

	a.Env = newEnv
	a.BuildArgs = c.FormValue("build_args")
	if err := h.store.UpdateTile(ctx, a); err != nil {
		return err
	}
	// Env vars only take effect at container start, so redeploy a running
	// service, otherwise a just-added ${secret.*} silently never connects.
	// Crons apply their env on the next scheduled run; volumes/dbs don't apply.
	// The rule itself is the deploy service's, in one place, where it used to
	// be spelled out here and in three other handlers.
	if a.Kind == "service" && !a.IsManaged() && !a.IsVolume() && a.Status == "running" {
		h.deploys.RedeployIfRunning(ctx, a.ID, "variables")
		middleware.SetFlash(c, "Variables saved. "+a.Name+" is redeploying to apply them.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "Variables saved. They apply on the next deploy.", middleware.FlashSuccess)
	}
	return h.panelDone(c, a, "variables")
}

// uiManaged reports whether the stack owns its tiles in the UI (not bound to a
// config repo), the stacks where structural edits stage instead of deploy.
func (h *handler) uiManaged(ctx context.Context, stackID string) bool {
	stack, err := h.stacks.Get(ctx, stackID)
	return err == nil && !stack.ConfigManaged()
}

// configMode is "" when the UI owns the stack outright, else the stack's
// ui_edits mode, what the panel does with an edit to a file-owned field.
func (h *handler) configMode(ctx context.Context, stackID string) string {
	s, err := h.stacks.Get(ctx, stackID)
	if err != nil || !s.ConfigManaged() {
		return ""
	}
	return s.UIEdits()
}

// editGate decides what happens to an edit to a field the config file owns.
// A UI-managed stack always stages. A config-managed one stages only when it
// declares ui_edits: stage, the panel then wins until the next config plan,
// which shows the drift and overwrites it on apply, terraform style. Anything
// else is refused, and a lookup error refuses too: this fails closed.
//
// The rule itself is GateService's — this was a second implementation of it,
// written before the gate existed, differing only in that it could not answer
// for a surface other than the canvas.
func (h *handler) editGate(ctx context.Context, stackID string) (stage bool, err error) {
	stage, err = h.gate.Gate(ctx, stackID, service.GateFieldEdit, service.SurfaceCanvas)
	if err != nil {
		return false, stackrmw.HTTP(err)
	}
	return stage, nil
}

// managedErr is the 409 returned for a blocked structural write on a
// config-managed stack. The file (not the UI/API) owns tiles/domains/envs,
// and a config plan would revert the change (the silent-drift bug). Secrets,
// certs and provisioning are not structural and never use this.
var managedErr = stackrmw.ManagedErr

// rejectManaged blocks a structural write when the config file owns the stack.
// Fails closed: a lookup error blocks rather than allows.
func (h *handler) rejectManaged(ctx context.Context, stackID string) error {
	s, err := h.stacks.Get(ctx, stackID)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if s.ConfigManaged() {
		return managedErr(s)
	}
	return nil
}

// actorOf names who pressed the button, for a run row's history line.
func actorOf(c echo.Context) string {
	if u := stackrmw.CurrentUser(c); u != nil {
		return u.Email
	}
	return ""
}

// stage records a pending change for the tile, stamped with the current user.
func (h *handler) stage(c echo.Context, a *repo.Tile, summary, op string, patch any) error {
	authorID, authorName := "", ""
	if u := stackrmw.CurrentUser(c); u != nil {
		authorID, authorName = u.ID, u.Name
	}
	return staging.Stage(c.Request().Context(), h.store, a, authorID, authorName, summary, op, patch)
}

// envMapFromBlob parses a KEY=VALUE blob into a map (always non-nil so an
// emptied blob stages as an explicit env clear, not an omitted field).
func envMapFromBlob(raw string) map[string]string {
	m := map[string]string{}
	for _, v := range envutil.Parse(raw) {
		m[v.Key] = v.Value
	}
	return m
}

// currentDesiredEnv returns the tile's desired env map, the pending "env vars"
// staged edit if one exists, else the committed blob. Deleting one name has to
// build on this: staging replaces the whole group per (tile, summary), so
// rebuilding from the committed blob would silently drop a staged env edit.
func (h *handler) currentDesiredEnv(ctx context.Context, a *repo.Tile) (map[string]string, error) {
	existing, err := h.store.ListStagedByEnv(ctx, a.EnvironmentID)
	if err != nil {
		return nil, err
	}
	for i := range existing {
		if existing[i].TileSlug != a.Slug || existing[i].Summary != "env vars" {
			continue
		}
		var p struct {
			Patch struct {
				Env map[string]string `json:"env"`
			} `json:"patch"`
		}
		if json.Unmarshal([]byte(existing[i].Payload), &p) == nil && p.Patch.Env != nil {
			return p.Patch.Env, nil
		}
	}
	return envMapFromBlob(a.Env), nil
}

// currentDesiredDomains returns the tile's desired domain set, the pending
// "domains" staged edit if one exists, else the committed domains serialized
// to config. Each staged domain mutation builds on this so successive edits
// compose (staging replaces the whole set, so a lone add would else clobber).
func (h *handler) currentDesiredDomains(ctx context.Context, a *repo.Tile) ([]stackconf.DomainConf, error) {
	if existing, err := h.store.ListStagedByEnv(ctx, a.EnvironmentID); err == nil {
		for i := range existing {
			if existing[i].TileSlug == a.Slug && existing[i].Summary == "domains" {
				var p struct {
					Patch struct {
						Domains []stackconf.DomainConf `json:"domains"`
					} `json:"patch"`
				}
				if json.Unmarshal([]byte(existing[i].Payload), &p) == nil {
					return p.Patch.Domains, nil
				}
			}
		}
	}
	cur, err := h.store.ListDomainsByTile(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	return stackconf.TileConfOf(stackconf.TileState{Tile: *a, Domains: cur}, stackconf.EnvState{}).Domains, nil
}

// splitNonEmpty splits newline-separated form text, dropping blank lines.
func splitNonEmpty(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// nonNegInt parses a form number, clamping absent/invalid/negative to 0.
func nonNegInt(v string) int {
	n, _ := strconv.Atoi(v)
	if n < 0 {
		return 0
	}
	return n
}

// POST /apps/:id/settings
//
// Binds the form onto the loaded tile and hands the whole row to the tile
// service. Every rule that used to live here — the cron expression, the git
// URL, the connector's org, the mount and device grammars, the replica
// guard, the limits — is the service's now, and so is deciding whether the
// save stages, rewrites the route or redeploys. This function maps strings
// to fields and renders the answer.
func (h *handler) SaveSettings(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	// Env vars are owned by the Variables tab (SaveEnv) and no longer live on
	// the settings form, so settings saves must never touch a.Env.
	switch a.Kind {
	case "cron":
		// cron tiles have a slim settings form: image, schedule, command, limits
		a.Cron = c.FormValue("cron")
		a.Command = c.FormValue("command")
		a.ImageRef = c.FormValue("image_ref")
		a.UpdatePolicy = c.FormValue("update_policy")
		a.AllowOverlap = c.FormValue("allow_overlap") != ""
		if err := bindInts(c, map[string]*int{"timeout_minutes": &a.TimeoutMinutes}); err != nil {
			return stackrmw.HTTP(err)
		}
		if err := bindLimits(c, a); err != nil {
			return stackrmw.HTTP(err)
		}
	case "function":
		// a cron minus the schedule, plus the on-deploy trigger
		a.Command = c.FormValue("command")
		a.ImageRef = c.FormValue("image_ref")
		a.UpdatePolicy = c.FormValue("update_policy")
		a.RunOnDeploy = c.FormValue("run_on_deploy") != ""
		a.DependsOn = c.FormValue("depends_on")
		a.AllowOverlap = c.FormValue("allow_overlap") != ""
		if err := bindInts(c, map[string]*int{"timeout_minutes": &a.TimeoutMinutes}); err != nil {
			return stackrmw.HTTP(err)
		}
		if err := bindLimits(c, a); err != nil {
			return stackrmw.HTTP(err)
		}
	default:
		a.SourceType = c.FormValue("source_type")
		a.GitURL = strings.TrimSpace(c.FormValue("git_url"))
		a.GitBranch = c.FormValue("git_branch")
		a.ConnectorID = c.FormValue("connector_id")
		a.ImageRef = c.FormValue("image_ref")
		a.DockerfilePath = c.FormValue("dockerfile_path")
		a.BuildContext = c.FormValue("build_context")
		a.BuildArgs = c.FormValue("build_args")
		a.Volumes = c.FormValue("volumes")
		a.Files = c.FormValue("files")
		a.WatchPaths = c.FormValue("watch_paths")
		a.HealthcheckCmd = c.FormValue("healthcheck_cmd")
		a.Command = c.FormValue("command")
		a.User = c.FormValue("user")
		a.Privileged = c.FormValue("privileged") != ""
		a.Devices = c.FormValue("devices")
		a.RestartPolicy = c.FormValue("restart_policy")
		a.DependsOn = c.FormValue("depends_on")
		a.PublishedPorts = c.FormValue("published_ports")
		a.TraefikOverride = c.FormValue("traefik_override")
		a.BasicAuthUser = c.FormValue("basic_auth_user")
		a.BasicAuthPassword = c.FormValue("basic_auth_password")
		a.SecHeaders = c.FormValue("sec_headers") != ""
		a.UpdatePolicy = c.FormValue("update_policy")
		a.WaitForCI = c.FormValue("wait_for_ci") != ""
		// These six render on every settings form, so an empty input is the
		// user clearing the field, not the form staying quiet about it.
		// Zeroed first, then bound: without this, clearing the port in the
		// panel is a silent no-op — no write, no diff, no redeploy.
		a.ContainerPort, a.ShmSizeMB = 0, 0
		a.HealthcheckIntervalS, a.HealthcheckTimeoutS = 0, 0
		a.HealthcheckRetries, a.HealthcheckStartPeriodS = 0, 0
		if err := bindInts(c, map[string]*int{
			"container_port":             &a.ContainerPort,
			"shm_size_mb":                &a.ShmSizeMB,
			"healthcheck_interval_s":     &a.HealthcheckIntervalS,
			"healthcheck_timeout_s":      &a.HealthcheckTimeoutS,
			"healthcheck_retries":        &a.HealthcheckRetries,
			"healthcheck_start_period_s": &a.HealthcheckStartPeriodS,
			// replicas is the exception and keeps skip-on-empty: the field
			// is absent from the form for kinds that cannot scale, and zero
			// is not a replica count anyone means.
			"replicas": &a.Replicas,
		}); err != nil {
			return stackrmw.HTTP(err)
		}
		if err := bindLimits(c, a); err != nil {
			return stackrmw.HTTP(err)
		}
		// Placement. Home node is deliberately not here: it is state, set on
		// first deploy and changed only by a completed volume move, and a
		// settings save that carried a stale value over it would point the
		// tile at an empty volume on another machine
		// (docs/plans/32-multi-node-ui.md, tile drawer).
		a.NodeGroup = strings.TrimSpace(c.FormValue("node_group"))
		if a.NodeGroup == "any" {
			a.NodeGroup = ""
		}
		// Form semantics, not a rule: each of these knobs renders only for
		// its own source, so on the other source the browser sends nothing
		// and a stale value left on the row would read as an edit nobody
		// made. The service still refuses the combination if a caller sends
		// it outright.
		if a.SourceType != "image" {
			a.UpdatePolicy = ""
		}
		if a.SourceType != "git" {
			a.WaitForCI = false
		}
	}
	staged, err := h.tiles.Update(c.Request().Context(), a, nil, stackrmw.WebActor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if staged {
		middleware.SetFlash(c, "Settings staged. Review & apply on the canvas to deploy.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "Settings saved.", middleware.FlashSuccess)
	}
	return h.panelDone(c, a, "settings")
}

// bindInts reads whole numbers off the form. An empty input leaves the field
// alone, so the caller zeroes anything the form always renders *before*
// calling — an empty box there is a clear, not a silence. A field that is
// present and not a number is a 400 naming the field, where it used to become
// a silent zero (SP1: validate, do not coerce).
func bindInts(c echo.Context, into map[string]*int) error {
	for key, dst := range into {
		raw := strings.TrimSpace(c.FormValue(key))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return svcerr.Invalid{Field: key, Msg: "must be a whole number"}
		}
		*dst = n
	}
	return nil
}

// bindLimits reads the cpu/memory pair. Both halves together, because they
// are written together: sending one alone used to zero the other.
func bindLimits(c echo.Context, a *repo.Tile) error {
	if raw := strings.TrimSpace(c.FormValue("cpu_limit")); raw != "" {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return svcerr.Invalid{Field: "cpu_limit", Msg: "must be a number, for example 0.5"}
		}
		a.CPULimit = f
	} else {
		a.CPULimit = 0
	}
	a.MemLimitMB = 0
	return bindInts(c, map[string]*int{"mem_limit_mb": &a.MemLimitMB})
}

// POST /apps/:id/delete
func (h *handler) Delete(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	// The typed name, re-checked here. The browser only disables a button,
	// and a confirmation is a property of this form, not of the write.
	if err := components.RequireConfirm(c, a.Slug); err != nil {
		return err
	}
	staged, err := h.tiles.Delete(c.Request().Context(), a, stackrmw.WebActor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if staged {
		// The tile stays on the canvas, struck through, until the pending set
		// is applied — which is what tears it down.
		middleware.SetFlash(c, "Deletion staged. Review & apply on the canvas to remove it.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "App deleted.", middleware.FlashSuccess)
	}
	return respond.Redirect(c, "/projects/"+a.StackID)
}

// POST /apps/:id/domains
func (h *handler) CreateDomain(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	var mws []string
	for _, m := range strings.Split(c.FormValue("middlewares"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			mws = append(mws, m)
		}
	}
	priority, _ := strconv.Atoi(c.FormValue("priority"))
	ownPort, _ := strconv.Atoi(c.FormValue("container_port"))
	// The form's HTTPS control is a checkbox, so an unticked box is an
	// explicit "off", not "unsaid". A JSON body or a config file that omits
	// the key means "on"; that is the difference the pointer carries.
	off := c.FormValue("https") == ""
	https := !off
	spec := service.DomainSpec{
		Host: c.FormValue("host"), Path: c.FormValue("path"), Port: ownPort,
		HTTPS: &https, RedirectTo: c.FormValue("redirect_to"),
		Rule: c.FormValue("rule"), Priority: priority, Middlewares: mws,
	}
	d, staged, err := h.domains.Attach(c.Request().Context(), a, spec, stackrmw.WebActor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if staged {
		if err := h.stageDomains(c, a, func(cur []stackconf.DomainConf) []stackconf.DomainConf {
			dc := stackconf.DomainConf{Host: d.Host, Path: d.Path, RedirectTo: d.RedirectTo,
				Rule: d.Rule, Priority: d.Priority, Middlewares: mws, Port: ownPort}
			if !d.HTTPS {
				// Both halves, explicitly. DomainConf.ForceHTTPSOn treats nil
				// as on, so carrying only https:false staged a domain that
				// applied with the redirect still enabled — bouncing plain
				// HTTP onto TLS that is not served.
				dc.HTTPS, dc.ForceHTTPS = &off, &off
			}
			return append(cur, dc)
		}); err != nil {
			return err
		}
		middleware.SetFlash(c, "Domain staged. Review & apply on the canvas.", middleware.FlashSuccess)
	}
	return h.panelDone(c, a, "settings")
}

// POST /apps/:id/domains/auto, give the tile its generated hostname under the
// nearest visible domain resource (same as config `auto: true`). Auto rows are
// generated, not authored, so they don't go through staging even on a
// UI-managed stack.
func (h *handler) CreateAutoDomain(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	env, err := h.envs.Get(ctx, a.EnvironmentID)
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if err := h.domains.AddAuto(ctx, env, a, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	// EnsureAuto is a silent no-op with no resource to nest under; say where
	// to add one rather than appearing to do nothing.
	ds, _ := h.store.ListDomainsByTile(ctx, a.ID)
	auto := false
	for _, d := range ds {
		if d.Auto {
			auto = true
			break
		}
	}
	if !auto {
		middleware.SetFlash(c, "No domain resource to nest under. Add one in the server, organization or stack domain settings first.", middleware.FlashError)
	} else {
		middleware.SetFlash(c, "Auto domain added.", middleware.FlashSuccess)
	}
	return h.panelDone(c, a, "settings")
}

// POST /apps/:id/domains/:domainID/https, flip HTTPS and re-sync the proxy.
func (h *handler) ToggleDomainHTTPS(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	d, on, staged, err := h.domains.ToggleHTTPS(c.Request().Context(), a, c.Param("domainID"), stackrmw.WebActor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if staged {
		if err := h.stageDomains(c, a, func(cur []stackconf.DomainConf) []stackconf.DomainConf {
			for i := range cur {
				if cur[i].Host == d.Host && cur[i].Path == d.Path {
					if on {
						cur[i].HTTPS = nil // nil is on
					} else {
						off := false
						cur[i].HTTPS = &off
					}
				}
			}
			return cur
		}); err != nil {
			return err
		}
		middleware.SetFlash(c, "Domain change staged. Review & apply on the canvas.", middleware.FlashSuccess)
	}
	return h.panelDone(c, a, "settings")
}

// POST /apps/:id/domains/:domainID/cert, set or clear a custom certificate.
func (h *handler) SetDomainCert(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	certPEM, keyPEM := c.FormValue("cert_pem"), c.FormValue("key_pem")
	if _, _, err := h.domains.SetTLS(c.Request().Context(), a, c.Param("domainID"),
		service.TLSPatch{CertPEM: &certPEM, KeyPEM: &keyPEM}, stackrmw.WebActor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	if strings.TrimSpace(certPEM) == "" {
		middleware.SetFlash(c, "Custom certificate removed.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "Custom certificate installed.", middleware.FlashSuccess)
	}
	return h.panelDone(c, a, "settings")
}

// POST /apps/:id/domains/:domainID/delete
func (h *handler) DeleteDomain(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	d, staged, err := h.domains.Detach(c.Request().Context(), a, c.Param("domainID"), stackrmw.WebActor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	if staged {
		if err := h.stageDomains(c, a, func(cur []stackconf.DomainConf) []stackconf.DomainConf {
			kept := cur[:0]
			for _, dc := range cur {
				if dc.Host != d.Host || dc.Path != d.Path {
					kept = append(kept, dc)
				}
			}
			return kept
		}); err != nil {
			return err
		}
		middleware.SetFlash(c, "Domain removal staged. Review & apply on the canvas.", middleware.FlashSuccess)
	}
	return h.panelDone(c, a, "settings")
}

// stageDomains records a change to the tile's desired domain set. The service
// has already decided the change is legal and what it looks like; this only
// puts it in the pending set, in the config engine's shape, which the service
// cannot build without importing the config engine.
func (h *handler) stageDomains(c echo.Context, a *repo.Tile, mutate func([]stackconf.DomainConf) []stackconf.DomainConf) error {
	desired, err := h.currentDesiredDomains(c.Request().Context(), a)
	if err != nil {
		return err
	}
	return h.stage(c, a, "domains", staging.OpUpdate, map[string]any{"domains": mutate(desired)})
}

// WithScheduler gives the handler the schedule reloader.
func (h *handler) WithScheduler(s *scheduler.Service) *handler { h.sched = s; return h }
