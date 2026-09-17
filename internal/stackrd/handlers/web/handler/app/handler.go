package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"


	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/config/envutil"
	"github.com/FyrmForge/stackr/internal/stackrd/config/staging"
	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/githubapp"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/audit"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/placement"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store    repo.Store
	clus     *cluster.Cluster // every docker call (docs/plans/35-cluster.md)
	px       *proxy.Proxy
	engine   *deploy.Engine
	jobs     *jobs.Service
	gh       *githubapp.Client
	notifier *notify.Notifier
}

// NewHandler creates a new app handler.
func NewHandler(store repo.Store, clus *cluster.Cluster, px *proxy.Proxy, engine *deploy.Engine, jobsSvc *jobs.Service, gh *githubapp.Client, notifier *notify.Notifier) *handler {
	return &handler{store: store, clus: clus, px: px, engine: engine, jobs: jobsSvc, gh: gh, notifier: notifier}
}

// GET /apps/:id/connectors, <option>s for the git connector select.
func (h *handler) Connectors(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var conns []repo.Connector
	if stack, err := h.store.GetStack(ctx, a.StackID); err == nil && stack != nil {
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
	// A service's logs are every replica on every node, collected through the
	// manager; a container's are one replica on one box. Cron tiles have no
	// long-lived service, so they fall through to the container path.
	var (
		ch   <-chan string
		stop func()
	)
	if name := envnet.ServiceFor(ctx, h.store, a); name != "" {
		ch, stop, err = h.clus.StreamServiceLogsMarked(ctx, name, 300)
	}
	if ch == nil {
		cs, _ := h.clus.ListByLabel(ctx, runtime.LabelApp, a.ID)
		if len(cs) == 0 {
			_, _ = fmt.Fprint(res, "data: O - no running container. Deploy first (cron tiles only log per run)\n\n")
			res.Flush()
			return nil
		}
		ch, stop, err = h.clus.StreamLogsMarked(ctx, h.clus.Self(ctx), cs[0].ID, 300)
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
	cpu, mem, rx, tx := components.MetricPoints(c.Request().Context(), h.store, "app:"+a.ID, dur)
	return respond.HTML(c, http.StatusOK, metricsFrag(a, key, cpu, mem, rx, tx))
}

// parseLimits parses resource caps; anything invalid or negative means 0
// (unlimited). Docker rejects memory caps under 6MB, so tiny values clamp up.
func parseLimits(cpu, mem string) (float64, int) {
	c, _ := strconv.ParseFloat(cpu, 64)
	if c < 0 {
		c = 0
	}
	m, _ := strconv.Atoi(mem)
	if m < 0 {
		m = 0
	}
	if m > 0 && m < 6 {
		m = 6
	}
	return c, m
}

// timeoutMinutes parses a runtime cap, clamped to [1, 1440]; default 30.
func timeoutMinutes(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 30
	}
	if n > 1440 {
		return 1440
	}
	return n
}

// updatePolicyForm normalizes the settings select to the stored enum.
func updatePolicyForm(v string) string {
	if v == "notify" || v == "auto" {
		return v
	}
	return "off"
}

// POST /apps/:id/run, run a cron-kind app immediately (in background).
func (h *handler) RunNow(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	// StartApp, not a bare goroutine: the run row has to exist before the
	// header renders, or the badge draws idle for a run that is already going.
	// No flash either, the header says "running" and the Runs tab has the row.
	if _, err := h.jobs.StartApp(c.Request().Context(), a.ID, jobs.TriggerManualWeb, actorOf(c)); err != nil {
		return err
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
	run, err := h.store.GetCronRun(c.Request().Context(), c.Param("run"))
	if err != nil {
		return err
	}
	if run == nil || run.Ref != "app:"+a.ID {
		return echo.NewHTTPError(http.StatusNotFound, "run not found")
	}
	if !h.jobs.Stop(c.Request().Context(), run.ID) {
		middleware.SetFlash(c, "That run already finished.", middleware.FlashInfo)
	}
	return h.headerDone(c, a)
}

// runWindow is how many runs the Runs tab lists and the Logs tab replays.
const runWindow = 20

// GET /apps/:id/runs/logs/stream, SSE: the tile's runs as one stream. A cron
// has no long-lived container, so there is nothing to follow between runs:
// what the recent runs stored replays oldest-first, then the run in flight (if
// any) is followed live off its own container, found by the stackr.run label.
//
// Every line carries the run's short id as a "tag|" prefix, LogView's multi
// mode renders that as a coloured chip and matches it in the search box, so
// typing a run id narrows the stream to that run. Stored output has no
// per-line time, so replayed lines all carry the run's start.
func (h *handler) RunsLogsStream(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	runs, err := h.store.ListCronRuns(ctx, "app:"+a.ID, runWindow)
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
func (h *handler) Stop(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	if err := h.clus.ScaleService(c.Request().Context(), envnet.ServiceFor(c.Request().Context(), h.store, a), 0); err != nil {
		return err
	}
	if err := h.store.UpdateTileStatus(c.Request().Context(), a.ID, "stopped"); err != nil {
		slog.Error("tile status not saved", "tile", a.ID, "status", "stopped", "error", err)
	}
	h.statusChanged(a)
	a.Status = "stopped"
	// No flash: SetFlash only surfaces on the *next* request, and the header
	// re-render already shows the new status badge in place.
	return h.headerDone(c, a)
}

// POST /apps/:id/restart, bounce the tile. Deliberately not a redeploy: no
// rebuild, no new image, the same spec re-rolled. For a service that is a
// forced update (scale 0 then 1 would drop the replica count a stopped tile
// is meant to keep).
func (h *handler) Restart(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := h.restartTile(ctx, a); err != nil {
		// Bounced but not back up: record what is actually true, or the tile
		// keeps claiming "running" over a dead one.
		if serr := h.store.UpdateTileStatus(ctx, a.ID, "stopped"); serr != nil {
			slog.Error("tile status not saved", "tile", a.ID, "status", "stopped", "error", serr)
		}
		h.statusChanged(a)
		return err
	}
	if err := h.store.UpdateTileStatus(ctx, a.ID, "running"); err != nil {
		slog.Error("tile status not saved", "tile", a.ID, "status", "running", "error", err)
	}
	h.statusChanged(a)
	a.Status = "running"
	return h.headerDone(c, a)
}

// restartTile bounces a tile in place: a forced service update, managed
// instances included. Deliberately not scale 0 then 1, a stopped tile is
// meant to keep its replica count. A tile that was scaled to zero comes back
// up.
func (h *handler) restartTile(ctx context.Context, a *repo.Tile) error {
	name := envnet.ServiceFor(ctx, h.store, a)
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "nothing deployed to restart")
	}
	// A pinned tile is forced to one replica by placement, so its configured
	// count is not what it comes back as; placement.For is the authority and
	// runs on the next deploy. One is right for everything with a volume.
	return h.clus.RestartService(ctx, name, a.Replicas)
}

// statusChanged tells open canvases to refetch. The acting tab gets its new
// badge from the header re-render; every other viewer only learns from this.
func (h *handler) statusChanged(a *repo.Tile) {
	h.notifier.Project(a.StackID)
	h.notifier.Containers()
}

// POST /apps/:id/cron/toggle, pause/resume a cron-kind app's schedule.
func (h *handler) ToggleCron(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	status := "paused"
	if a.Status == "paused" {
		status = "idle"
	}
	if err := h.store.UpdateTileStatus(c.Request().Context(), a.ID, status); err != nil {
		return err
	}
	_ = h.jobs.LoadSchedules(c.Request().Context())
	h.statusChanged(a)
	a.Status = status
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
	a, err := h.store.GetTile(c.Request().Context(), c.Param("id"))
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "app not found")
	}
	if err := stackrmw.RequireStackAccess(c, h.store, a.StackID); err != nil {
		return nil, err
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
		d.openRun, _ = h.store.OpenCronRun(ctx, "app:"+a.ID)
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
			d.runs, _ = h.store.ListCronRuns(ctx, "app:"+a.ID, runWindow)
		}
	case "runs":
		d.runs, _ = h.store.ListCronRuns(ctx, "app:"+a.ID, runWindow)
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
		d.cpu, d.mem, d.rx, d.tx = components.MetricPoints(ctx, h.store, "app:"+a.ID, dur)
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
	stack, _ := h.store.GetStack(ctx, a.StackID)
	env, _ := h.store.GetEnvironment(ctx, a.EnvironmentID)
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
	r, _ := h.store.OpenCronRun(c.Request().Context(), "app:"+a.ID)
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
	tiles, err := h.store.ListTilesByEnv(ctx, a.EnvironmentID)
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
		target, err := h.store.GetTile(ctx, targetID)
		if err != nil || target == nil || target.EnvironmentID != a.EnvironmentID || target.Kind != "service" || target.IsManaged() {
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
		if stored, err := h.store.GetTile(ctx, a.ID); err == nil && stored != nil {
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
		if t, err := h.store.GetTile(ctx, id); err == nil && t != nil {
			_, _ = h.engine.Enqueue(ctx, t, "volume")
		}
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
		if t, _ := h.store.GetTile(ctx, p.InstanceTileID); t != nil {
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
	st, err := h.store.GetStack(ctx, instance.StackID)
	if err != nil || st == nil {
		return instance.Slug
	}
	org, err := h.store.GetOrg(ctx, st.OrgID)
	if err != nil || org == nil {
		return instance.Slug
	}
	return managedtiles.InfraPath(instance.ScopeKind, org.Slug, sc.StackSlug, sc.EnvSlug, instance.Slug)
}

func (h *handler) wireProvision(ctx context.Context, a *repo.Tile, instance *repo.Tile, p *repo.Provision, varName, verb string) (string, error) {
	ref := managedtiles.Ref(instance, p, managedtiles.DefaultOutput(instance.Engine))
	// On a config-managed stack the file owns tile.Env: injecting here would be
	// stripped by the next apply, leaving the app without its DB URL. The file
	// can declare the whole relationship with uses:, so point at that rather
	// than at a hand-written reference the next plan would show as drift.
	// !uiManaged also covers the lookup-error case: don't inject.
	if !h.uiManaged(ctx, a.StackID) {
		if varName == "" {
			varName = managedtiles.DefaultOutput(instance.Engine)
		}
		return "Database " + verb + ". Declare it in your config file so a plan can see it:\n" +
			"uses:\n  - infra: " + h.infraAddress(ctx, instance) + "\n    name: " + p.DBName +
			"\n    var: " + varName, nil
	}
	if varName == "" {
		return "Database " + verb + ". Add " + ref + " to this tile's variables; it applies on the next deploy.", nil
	}
	newEnv, used := injectEnvVar(a.Env, varName, ref)
	a.Env = newEnv
	if err := h.store.UpdateTile(ctx, a); err != nil {
		return "", err
	}
	if _, err := h.engine.Enqueue(ctx, a, "provision"); err != nil {
		return "", err
	}
	msg := "Database " + verb + ". " + used + " set and " + a.Name + " is redeploying."
	if used != varName {
		msg = "Database " + verb + ". " + varName + " was already used, so " + used + " was set instead; " + a.Name + " is redeploying."
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
	instance, err := h.store.GetTile(ctx, c.FormValue("instance_id"))
	if err != nil || instance == nil || !managedtiles.Eligible(ctx, h.store, instance, a) {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid shared instance")
	}
	p, err := h.dbsvc().Provision(ctx, instance, a, "", false)
	if err != nil {
		middleware.SetFlash(c, "Provisioning failed: "+err.Error(), middleware.FlashError)
		return h.renderProvisions(c, a)
	}
	msg, err := h.wireProvision(ctx, a, instance, p, envVarName(c.FormValue("env_var")), "provisioned")
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
	if err != nil || src == nil || src.EnvID != a.EnvironmentID {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid database")
	}
	instance, err := h.store.GetTile(ctx, src.InstanceTileID)
	if err != nil || instance == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "instance not found")
	}
	p, err := h.dbsvc().AttachExisting(ctx, instance, src, a)
	if err != nil {
		middleware.SetFlash(c, "Attach failed: "+err.Error(), middleware.FlashError)
		return h.renderProvisions(c, a)
	}
	msg, err := h.wireProvision(ctx, a, instance, p, envVarName(c.FormValue("env_var")), "attached")
	if err != nil {
		return err
	}
	middleware.SetFlash(c, msg, middleware.FlashSuccess)
	return h.renderProvisions(c, a)
}

// envVarName sanitises a user-supplied env var name to a safe shell
// identifier ("" if nothing usable remains).
func envVarName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r == '_':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32) // upper-case, env-var convention
		case r >= '0' && r <= '9' && i > 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// injectEnvVar sets key=val in the consumer's env without clobbering an
// existing different value. See envutil.Inject.
func injectEnvVar(env, key, val string) (string, string) {
	return envutil.Inject(env, key, val)
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
	p, err := h.store.GetProvision(ctx, c.Param("pid"))
	if err != nil || p == nil || p.ConsumerTileID != a.ID {
		return echo.NewHTTPError(http.StatusNotFound, "provision not found")
	}
	if err := h.dbsvc().Detach(ctx, p); err != nil {
		return err
	}
	middleware.SetFlash(c, "Detached. The data was kept. Drop it from the instance's panel.", middleware.FlashSuccess)
	return h.renderProvisions(c, a)
}

// dbsvc is the shared-instance provisioner (stateless, built on demand).
func (h *handler) dbsvc() *managedtiles.Service { return managedtiles.NewService(h.clus, h.store) }

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
	vars, err := h.store.ListVariables(ctx, repo.OwnerTile, a.ID)
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
	// own org. Checked explicitly: the access check above only refuses
	// viewers on mutating requests, and this is a GET.
	s, err := h.store.GetStack(c.Request().Context(), a.StackID)
	if err != nil || s == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if !stackrmw.CanWriteOrg(c, h.store, s.OrgID) {
		return echo.NewHTTPError(http.StatusForbidden, "read-only")
	}
	vars, err := h.store.ListVariables(c.Request().Context(), repo.OwnerTile, a.ID)
	if err != nil {
		return err
	}
	return audit.ServeValue(c, h.store, vars, repo.OwnerTile, a.ID, audit.Reveal)
}

// POST /apps/:id/vars/secret, add or replace one secret variable. Secrets are
// written as rows only: the raw env editor is rendered in plain text and its
// contents are diffed into config plans.
func (h *handler) SaveSecretVar(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(c.FormValue("name"))
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name required")
	}
	now := time.Now().UTC()
	if err := h.store.UpsertVariable(c.Request().Context(), &repo.Variable{
		OwnerKind: repo.OwnerTile, OwnerID: a.ID, Name: name,
		Value: components.VarValue(c), Secret: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		return err
	}
	audit.Record(c.Request().Context(), h.store, audit.Actor(c), audit.Set, repo.OwnerTile, a.ID, name)
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
	if vars, verr := h.store.ListVariables(ctx, repo.OwnerTile, a.ID); verr == nil {
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
	if err := h.store.DeleteVariable(ctx, repo.OwnerTile, a.ID, name); err != nil {
		return err
	}
	audit.Record(ctx, h.store, audit.Actor(c), audit.Delete, repo.OwnerTile, a.ID, name)
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
	if a.Kind == "service" && !a.IsManaged() && !a.IsVolume() && a.Status == "running" {
		if _, err := h.engine.Enqueue(ctx, a, "variables"); err != nil {
			return err
		}
		middleware.SetFlash(c, "Variables saved. "+a.Name+" is redeploying to apply them.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "Variables saved. They apply on the next deploy.", middleware.FlashSuccess)
	}
	return h.panelDone(c, a, "variables")
}

// uiManaged reports whether the stack owns its tiles in the UI (not bound to a
// config repo), the stacks where structural edits stage instead of deploy.
func (h *handler) uiManaged(ctx context.Context, stackID string) bool {
	stack, err := h.store.GetStack(ctx, stackID)
	return err == nil && stack != nil && !stack.ConfigManaged()
}

// configMode is "" when the UI owns the stack outright, else the stack's
// ui_edits mode, what the panel does with an edit to a file-owned field.
func (h *handler) configMode(ctx context.Context, stackID string) string {
	s, err := h.store.GetStack(ctx, stackID)
	if err != nil || s == nil || !s.ConfigManaged() {
		return ""
	}
	return s.UIEdits()
}

// editGate decides what happens to an edit to a field the config file owns.
// A UI-managed stack always stages. A config-managed one stages only when it
// declares ui_edits: stage, the panel then wins until the next config plan,
// which shows the drift and overwrites it on apply, terraform style. Anything
// else is refused, and a lookup error refuses too: this fails closed.
func (h *handler) editGate(ctx context.Context, stackID string) (stage bool, err error) {
	s, err := h.store.GetStack(ctx, stackID)
	if err != nil || s == nil {
		return false, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if s.ConfigManaged() && s.UIEdits() != repo.UIEditsStage {
		return false, managedErr(s)
	}
	return true, nil
}

// managedErr is the 409 returned for a blocked structural write on a
// config-managed stack. The file (not the UI/API) owns tiles/domains/envs,
// and a config plan would revert the change (the silent-drift bug). Secrets,
// certs and provisioning are not structural and never use this.
var managedErr = stackrmw.ManagedErr

// rejectManaged blocks a structural write when the config file owns the stack.
// Fails closed: a lookup error blocks rather than allows.
func (h *handler) rejectManaged(ctx context.Context, stackID string) error {
	s, err := h.store.GetStack(ctx, stackID)
	if err != nil || s == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
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

// settingsPatch builds the sparse config patch for a staged settings edit from
// the (in-memory, form-populated) tile. It carries every settings-owned field
// so a cleared field applies, and deliberately omits env + domains, those keep
// their own staged groups (SaveEnv / the domain handlers).
func settingsPatch(a *repo.Tile) map[string]any {
	p := map[string]any{"limits": map[string]any{"cpu": a.CPULimit, "memory_mb": a.MemLimitMB}}
	if a.Kind == "cron" {
		p["type"] = "cron"
		p["image"] = a.ImageRef
		p["schedule"] = a.Cron
		p["command"] = a.Command
		p["update_policy"] = a.UpdatePolicy
		p["allow_overlap"] = a.AllowOverlap
		p["timeout_minutes"] = a.TimeoutMinutes
		return p
	}
	if a.Kind == "function" {
		p["type"] = "function"
		p["image"] = a.ImageRef
		p["command"] = a.Command
		p["update_policy"] = a.UpdatePolicy
		p["run_on_deploy"] = a.RunOnDeploy
		p["allow_overlap"] = a.AllowOverlap
		p["timeout_minutes"] = a.TimeoutMinutes
		p["depends_on"] = splitNonEmpty(a.DependsOn)
		return p
	}
	p["port"] = a.ContainerPort
	p["healthcheck"] = a.HealthcheckCmd
	p["healthcheck_interval"] = a.HealthcheckIntervalS
	p["healthcheck_timeout"] = a.HealthcheckTimeoutS
	p["healthcheck_retries"] = a.HealthcheckRetries
	p["healthcheck_start_period"] = a.HealthcheckStartPeriodS
	p["command"] = a.Command
	p["user"] = a.User
	p["shm_size_mb"] = a.ShmSizeMB
	p["privileged"] = a.Privileged
	p["devices"] = splitNonEmpty(a.Devices)
	p["restart"] = a.RestartPolicy
	p["depends_on"] = splitNonEmpty(a.DependsOn)
	p["security_headers"] = a.SecHeaders
	p["volumes"] = splitNonEmpty(a.Volumes)
	p["files"] = splitNonEmpty(a.Files)
	p["storage"] = splitNonEmpty(a.Storage)
	p["watch_paths"] = splitNonEmpty(a.WatchPaths)
	p["build_args"] = a.BuildArgs
	p["published_ports"] = a.PublishedPorts
	p["traefik_override"] = a.TraefikOverride
	p["update_policy"] = a.UpdatePolicy
	p["wait_for_ci"] = a.WaitForCI
	p["basic_auth_user"] = a.BasicAuthUser
	p["basic_auth_password"] = a.BasicAuthPassword
	switch a.SourceType {
	case "image":
		p["image"] = a.ImageRef
	default: // git
		p["image"] = ""
		p["git_url"] = a.GitURL
		p["connector"] = a.ConnectorID
		p["branch"] = a.GitBranch
		p["build"] = map[string]any{"context": a.BuildContext, "dockerfile": a.DockerfilePath}
	}
	return p
}

// POST /apps/:id/settings
func (h *handler) SaveSettings(c echo.Context) error {
	ctx := c.Request().Context()
	a, err := h.load(c)
	if err != nil {
		return err
	}
	// The settings form is entirely fields the config file owns, so one gate
	// covers the whole save. Build args, push-to-registry, published ports,
	// the traefik override, basic auth and allow-overlap used to pass straight
	// through on a config-managed stack, every one of them is diffed by
	// plan.go, so those saves were silent drift the next plan reverted.
	stage, err := h.editGate(ctx, a.StackID)
	if err != nil {
		return err
	}
	// Env vars are owned by the Variables tab (SaveEnv) and no longer live on
	// the settings form, so settings saves must never touch a.Env.
	switch a.Kind {
	case "cron":
		// cron services have a slim settings form: image, schedule, command, env
		cronExpr := c.FormValue("cron")
		if err := jobs.ValidateCron(cronExpr); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid cron expression")
		}
		a.Cron = cronExpr
		a.Command = c.FormValue("command")
		a.ImageRef = c.FormValue("image_ref")
		a.UpdatePolicy = updatePolicyForm(c.FormValue("update_policy"))
		a.AllowOverlap = c.FormValue("allow_overlap") != ""
		a.TimeoutMinutes = timeoutMinutes(c.FormValue("timeout_minutes"))
		a.CPULimit, a.MemLimitMB = parseLimits(c.FormValue("cpu_limit"), c.FormValue("mem_limit_mb"))
	case "function":
		// a cron minus the schedule, plus the on-deploy trigger
		a.Command = c.FormValue("command")
		a.ImageRef = c.FormValue("image_ref")
		a.UpdatePolicy = updatePolicyForm(c.FormValue("update_policy"))
		a.RunOnDeploy = c.FormValue("run_on_deploy") != ""
		a.DependsOn = c.FormValue("depends_on")
		for _, l := range splitNonEmpty(a.DependsOn) {
			if _, _, err := stackconf.ParseDep(l); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
		}
		a.AllowOverlap = c.FormValue("allow_overlap") != ""
		a.TimeoutMinutes = timeoutMinutes(c.FormValue("timeout_minutes"))
		a.CPULimit, a.MemLimitMB = parseLimits(c.FormValue("cpu_limit"), c.FormValue("mem_limit_mb"))
	default:
		a.SourceType = c.FormValue("source_type")
		a.GitURL = strings.TrimSpace(c.FormValue("git_url"))
		if a.SourceType == "git" && !repo.ValidGitURL(a.GitURL) {
			return echo.NewHTTPError(http.StatusBadRequest, "Use a GitHub URL: https://github.com/owner/repo or git@github.com:owner/repo")
		}
		a.GitBranch = c.FormValue("git_branch")
		a.ConnectorID = c.FormValue("connector_id")
		// A connector grants credentials, only accept one from the tile's own org.
		if a.ConnectorID != "" {
			cn, err := h.store.GetConnector(c.Request().Context(), a.ConnectorID)
			ok := err == nil && cn != nil
			if ok {
				stack, serr := h.store.GetStack(c.Request().Context(), a.StackID)
				ok = serr == nil && stack != nil && stack.OrgID == cn.OrgID
			}
			if !ok {
				a.ConnectorID = ""
			}
		}
		a.ImageRef = c.FormValue("image_ref")
		a.DockerfilePath = c.FormValue("dockerfile_path")
		a.BuildContext = c.FormValue("build_context")
		a.BuildArgs = c.FormValue("build_args")
		a.Volumes = c.FormValue("volumes")
		a.Files = c.FormValue("files")
		for _, l := range splitNonEmpty(a.Files) {
			if _, _, _, err := runtime.ParseFileMount(l); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
		}
		a.WatchPaths = c.FormValue("watch_paths")
		a.HealthcheckCmd = c.FormValue("healthcheck_cmd")
		a.HealthcheckIntervalS = nonNegInt(c.FormValue("healthcheck_interval_s"))
		a.HealthcheckTimeoutS = nonNegInt(c.FormValue("healthcheck_timeout_s"))
		a.HealthcheckRetries = nonNegInt(c.FormValue("healthcheck_retries"))
		a.HealthcheckStartPeriodS = nonNegInt(c.FormValue("healthcheck_start_period_s"))
		a.Command = c.FormValue("command")
		a.User = c.FormValue("user")
		a.ShmSizeMB, _ = strconv.Atoi(c.FormValue("shm_size_mb"))
		if a.ShmSizeMB < 0 {
			a.ShmSizeMB = 0
		}
		a.Privileged = c.FormValue("privileged") != ""
		a.Devices = c.FormValue("devices")
		for _, l := range splitNonEmpty(a.Devices) {
			if _, err := runtime.ParseDevice(l); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
		}
		rp, err := runtime.NormalizeRestart(c.FormValue("restart_policy"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		a.RestartPolicy = rp
		a.DependsOn = c.FormValue("depends_on")
		for _, l := range splitNonEmpty(a.DependsOn) {
			if _, _, err := stackconf.ParseDep(l); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
		}
		// Placement. Home node is deliberately not here: it is state, set on
		// first deploy and changed only by a completed volume move, and a
		// settings save that carried a stale value over it would point the
		// tile at an empty volume on another machine
		// (docs/plans/32-multi-node-ui.md, tile drawer).
		if v := c.FormValue("replicas"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return echo.NewHTTPError(http.StatusBadRequest, "replicas: a whole number, at least 1")
			}
			if n > 1 && placement.IsPinned(ctx, h.store, a) {
				return echo.NewHTTPError(http.StatusBadRequest,
					"this tile holds a volume, so it can only run one replica: two writers on one volume corrupt it")
			}
			a.Replicas = n
		}
		a.NodeGroup = strings.TrimSpace(c.FormValue("node_group"))
		if a.NodeGroup == "any" {
			a.NodeGroup = ""
		}
		a.ContainerPort, _ = strconv.Atoi(c.FormValue("container_port"))
		a.UpdatePolicy = updatePolicyForm(c.FormValue("update_policy"))
		a.WaitForCI = c.FormValue("wait_for_ci") != ""
		// Each knob renders only for its source; a source switch in the same
		// save must not smuggle the other one along.
		if a.SourceType != "image" {
			a.UpdatePolicy = "off"
		}
		if a.SourceType != "git" {
			a.WaitForCI = false
		}
		a.CPULimit, a.MemLimitMB = parseLimits(c.FormValue("cpu_limit"), c.FormValue("mem_limit_mb"))
		a.SecHeaders = c.FormValue("sec_headers") != ""
		a.PublishedPorts = c.FormValue("published_ports")
		a.TraefikOverride = c.FormValue("traefik_override")
		a.BasicAuthUser = c.FormValue("basic_auth_user")
		a.BasicAuthPassword = c.FormValue("basic_auth_password")
		if a.BasicAuthUser == "" {
			a.BasicAuthPassword = ""
		} else if a.BasicAuthPassword == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "basic auth password required")
		}
	}
	// The settings edit goes into the per-env pending set; env + domains keep
	// their own staged groups. Compose tiles used to be exempt because the
	// config engine could not model them; it can now, so they stage too.
	if stage {
		if err := h.stage(c, a, "settings", staging.OpUpdate, settingsPatch(a)); err != nil {
			return err
		}
		middleware.SetFlash(c, "Settings staged. Review & apply on the canvas to deploy.", middleware.FlashSuccess)
		return h.panelDone(c, a, "settings")
	}
	if err := h.store.UpdateTile(c.Request().Context(), a); err != nil {
		return err
	}
	if a.Kind == "cron" {
		_ = h.jobs.LoadSchedules(c.Request().Context())
	}
	// Routing extras (auth, headers, override) live in the Traefik config,
	// re-render it so they apply without a redeploy.
	if a.Kind != "cron" && a.Kind != "function" {
		if err := h.syncProxy(c, a); err != nil {
			return err
		}
	}
	middleware.SetFlash(c, "Settings saved.", middleware.FlashSuccess)
	return h.panelDone(c, a, "settings")
}

// POST /apps/:id/delete
func (h *handler) Delete(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	stage, err := h.editGate(ctx, a.StackID)
	if err != nil {
		return err
	}
	if a.IsVolume() && a.AttachedTileID != "" {
		return echo.NewHTTPError(http.StatusBadRequest, "detach the volume before deleting it")
	}
	// The typed name, re-checked here. The browser only disables a button.
	if err := components.RequireConfirm(c, a.Slug); err != nil {
		return err
	}
	// Stage the deletion. The tile stays on the canvas (struck through) until
	// the pending set is applied, which tears it down.
	if stage {
		if err := h.stage(c, a, "delete", staging.OpDelete, nil); err != nil {
			return err
		}
		middleware.SetFlash(c, "Deletion staged. Review & apply on the canvas to remove it.", middleware.FlashSuccess)
		return respond.Redirect(c, "/projects/"+a.StackID)
	}
	envnet.TearDown(ctx, h.store, h.clus, a)
	_ = h.px.RemoveApp(a.ID)
	// Orphan (don't drop) any shared-db provisions this service owned.
	if ps, _ := h.store.ListProvisionsByConsumer(ctx, a.ID); len(ps) > 0 {
		for i := range ps {
			_ = h.dbsvc().Detach(ctx, &ps[i])
		}
	}
	if err := h.store.DeleteTile(ctx, a.ID); err != nil {
		return err
	}
	middleware.SetFlash(c, "App deleted.", middleware.FlashSuccess)
	return respond.Redirect(c, "/projects/"+a.StackID)
}

// POST /apps/:id/domains
func (h *handler) CreateDomain(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	stage, err := h.editGate(ctx, a.StackID)
	if err != nil {
		return err
	}
	redirectTo := strings.TrimSpace(c.FormValue("redirect_to"))
	rule := strings.TrimSpace(c.FormValue("rule"))
	priority, _ := strconv.Atoi(c.FormValue("priority"))
	var mws []string
	for _, m := range strings.Split(c.FormValue("middlewares"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			mws = append(mws, m)
		}
	}
	// traefik disables a router naming a middleware it cannot find and says
	// so only in its own log, so refuse the typo here, like the config plan.
	if err := h.checkMiddlewares(ctx, a.StackID, mws); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	port, _ := strconv.Atoi(c.FormValue("container_port"))
	ownPort := port
	if port == 0 {
		port = a.ContainerPort
	}
	if port == 0 {
		if redirectTo == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "container port required (set it on the app or the domain)")
		}
		port = 80 // redirect domains never proxy; the service target is unused
	}
	host := c.FormValue("host")
	if host == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "host required")
	}
	if strings.HasPrefix(host, "*.") && c.FormValue("https") != "" {
		if p, _ := h.store.GetSetting(ctx, "dns_provider"); p == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "wildcard HTTPS needs a DNS provider; configure it in Settings")
		}
	}
	// Normalize empty path to "/" (matches the API and the unique index) and
	// reject a host+path already claimed by any tile, one route, one owner.
	path := c.FormValue("path")
	if path == "" {
		path = "/"
	}
	// A rule entry may share host + path with this tile's own entries, never
	// with another tile's: its priority would take that tile's traffic.
	if existing, _ := h.store.GetDomainByHostPath(ctx, host, path); existing != nil && (existing.TileID != a.ID || (rule == "" && existing.Rule == "")) {
		return echo.NewHTTPError(http.StatusConflict, "host + path already in use")
	}
	// Anti-squat: a custom host may not start with another org's slug, the same
	// rule org domain resources are under. Without it the guard on the org page
	// means nothing, since anyone can claim the same name one tile down.
	stack, err := h.store.GetStack(ctx, a.StackID)
	if err != nil || stack == nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "could not read the stack this tile belongs to")
	}
	if err := envops.CheckOrgSquat(ctx, h.store, host, stack.OrgID); err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	// Stage the domain onto the tile's desired set.
	if stage {
		desired, err := h.currentDesiredDomains(ctx, a)
		if err != nil {
			return err
		}
		dc := stackconf.DomainConf{Host: host, Path: path, RedirectTo: redirectTo,
			Rule: rule, Priority: priority, Middlewares: mws, Port: ownPort}
		if c.FormValue("https") == "" {
			off := false
			dc.HTTPS = &off
		}
		desired = append(desired, dc)
		if err := h.stage(c, a, "domains", staging.OpUpdate, map[string]any{"domains": desired}); err != nil {
			return err
		}
		middleware.SetFlash(c, "Domain staged. Review & apply on the canvas.", middleware.FlashSuccess)
		return h.panelDone(c, a, "settings")
	}
	d := &repo.Domain{
		ID:            uuid.New().String(),
		TileID:        a.ID,
		Host:          host,
		Path:          path,
		ContainerPort: port,
		HTTPS:         c.FormValue("https") != "",
		RedirectTo:    redirectTo,
		Rule:          rule,
		Priority:      priority,
		Middlewares:   strings.Join(mws, "\n"),
		CreatedAt:     time.Now().UTC(),
	}
	if err := h.store.CreateDomain(ctx, d); err != nil {
		return err
	}
	if err := h.syncProxy(c, a); err != nil {
		return err
	}
	return h.panelDone(c, a, "settings")
}

// POST /apps/:id/domains/auto, give the tile its generated hostname under the
// nearest visible domain resource (envops.EnsureAutoDomain, same as config
// `auto: true`). Auto rows are generated, not authored, so they don't go
// through staging even on a UI-managed stack.
func (h *handler) CreateAutoDomain(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	if err := h.rejectManaged(ctx, a.StackID); err != nil {
		return err
	}
	if a.Kind != "service" || a.ContainerPort == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "auto domains need a service with a container port")
	}
	env, err := h.store.GetEnvironment(ctx, a.EnvironmentID)
	if err != nil || env == nil {
		return echo.NewHTTPError(http.StatusNotFound, "environment not found")
	}
	if err := (envops.Ops{Store: h.store, RT: h.clus.Runtime(), Cluster: h.clus, PX: h.px}).EnsureAutoDomain(ctx, env, a); err != nil {
		return err
	}
	// EnsureAutoDomain is a silent no-op with no resource to nest under, tell
	// the operator where to add one instead of appearing to do nothing.
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
	ctx := c.Request().Context()
	stage, err := h.editGate(ctx, a.StackID)
	if err != nil {
		return err
	}
	d, err := h.store.GetDomain(ctx, c.Param("domainID"))
	if err != nil || d == nil {
		return echo.NewHTTPError(http.StatusNotFound, "domain not found")
	}
	// Stage the HTTPS flip onto the tile's desired domain set.
	if stage {
		desired, err := h.currentDesiredDomains(ctx, a)
		if err != nil {
			return err
		}
		for i := range desired {
			if desired[i].Host == d.Host && desired[i].Path == d.Path {
				if d.HTTPS { // was on → now off
					off := false
					desired[i].HTTPS = &off
				} else { // was off → now on (nil = on)
					desired[i].HTTPS = nil
				}
			}
		}
		if err := h.stage(c, a, "domains", staging.OpUpdate, map[string]any{"domains": desired}); err != nil {
			return err
		}
		middleware.SetFlash(c, "Domain change staged. Review & apply on the canvas.", middleware.FlashSuccess)
		return h.panelDone(c, a, "settings")
	}
	if err := h.store.SetDomainHTTPS(ctx, d.ID, !d.HTTPS); err != nil {
		return err
	}
	if err := h.syncProxy(c, a); err != nil {
		return err
	}
	return h.panelDone(c, a, "settings")
}

// POST /apps/:id/domains/:domainID/cert, set or clear a custom certificate.
func (h *handler) SetDomainCert(c echo.Context) error {
	a, err := h.load(c)
	if err != nil {
		return err
	}
	d, err := h.store.GetDomain(c.Request().Context(), c.Param("domainID"))
	if err != nil || d == nil || d.TileID != a.ID {
		return echo.NewHTTPError(http.StatusNotFound, "domain not found")
	}
	certPEM := strings.TrimSpace(c.FormValue("cert_pem"))
	keyPEM := strings.TrimSpace(c.FormValue("key_pem"))
	if certPEM != "" || keyPEM != "" {
		if _, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid certificate/key pair: "+err.Error())
		}
	}
	if err := h.store.SetDomainCert(c.Request().Context(), d.ID, certPEM, keyPEM); err != nil {
		return err
	}
	if err := h.syncProxy(c, a); err != nil {
		return err
	}
	if certPEM == "" {
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
	ctx := c.Request().Context()
	stage, err := h.editGate(ctx, a.StackID)
	if err != nil {
		return err
	}
	// Stage the removal, drop the domain from the desired set.
	if stage {
		d, err := h.store.GetDomain(ctx, c.Param("domainID"))
		if err != nil || d == nil || d.TileID != a.ID {
			return echo.NewHTTPError(http.StatusNotFound, "domain not found")
		}
		desired, err := h.currentDesiredDomains(ctx, a)
		if err != nil {
			return err
		}
		kept := desired[:0]
		for _, dc := range desired {
			if dc.Host != d.Host || dc.Path != d.Path {
				kept = append(kept, dc)
			}
		}
		if err := h.stage(c, a, "domains", staging.OpUpdate, map[string]any{"domains": kept}); err != nil {
			return err
		}
		middleware.SetFlash(c, "Domain removal staged. Review & apply on the canvas.", middleware.FlashSuccess)
		return h.panelDone(c, a, "settings")
	}
	if err := h.store.DeleteDomain(ctx, c.Param("domainID")); err != nil {
		return err
	}
	if err := h.syncProxy(c, a); err != nil {
		return err
	}
	return h.panelDone(c, a, "settings")
}

func (h *handler) syncProxy(c echo.Context, a *repo.Tile) error {
	domains, err := h.store.ListDomainsByTile(c.Request().Context(), a.ID)
	if err != nil {
		return err
	}
	return h.px.WriteApp(a, domains)
}

// checkMiddlewares refuses a middleware reference no stack file declares: a
// bare name is the tile's own stack's, stack/name another stack's in the org.
func (h *handler) checkMiddlewares(ctx context.Context, stackID string, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	own, err := h.store.GetStack(ctx, stackID)
	if err != nil || own == nil {
		return fmt.Errorf("stack not found")
	}
	for _, ref := range refs {
		slug, name, cross := strings.Cut(ref, "/")
		st := own
		if cross {
			if st, err = h.store.GetStackBySlug(ctx, own.OrgID, slug); err != nil || st == nil {
				return fmt.Errorf("no stack %s in this org", slug)
			}
		} else {
			name = slug
		}
		if _, ok := stackconf.ParseMiddlewares(st.ProxyMiddlewares)[name]; !ok {
			return fmt.Errorf("no middleware %s; proxy.middlewares in the stack file declares them", ref)
		}
	}
	return nil
}
