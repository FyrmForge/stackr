package db

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	svcproxy "github.com/FyrmForge/stackr/internal/stackrd/service/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store     repo.Store
	dbs       *managedtiles.Service
	px        *svcproxy.Service
	notifier  *notify.Notifier
	clus      *cluster.Cluster // every docker call: a database's data volume is on its home node's disk, not necessarily the manager's
	instances *service.ManagedInstanceService
	slices    *service.SliceService
	life      *service.TileLifecycleService
	domains   *service.DomainService
	// telemetry owns the metric window: the bucketing used to sit in a templ
	// file reading the store (point 19).
	telemetry *service.TileTelemetryService
}

// NewHandler creates a new database handler.
func NewHandler(store repo.Store, dbs *managedtiles.Service, clus *cluster.Cluster, px *svcproxy.Service, notifier *notify.Notifier) *handler {
	return &handler{store: store, dbs: dbs, clus: clus, px: px, notifier: notifier}
}

// WithServices gives the panel the same values the API holds, so an action
// taken here and the identical call over the API perform the same effects.
func (h *handler) WithServices(inst *service.ManagedInstanceService, sl *service.SliceService,
	life *service.TileLifecycleService, dom *service.DomainService,
	tel *service.TileTelemetryService) *handler {
	h.instances, h.slices, h.life, h.domains, h.telemetry = inst, sl, life, dom, tel
	return h
}

// actor names the signed-in user. Via is deliberately left empty rather than
// set to "web": a managed instance has no staged shape, so every one of these
// writes goes straight through and the gate is only ever a refusal.
func (h *handler) actor(c echo.Context) service.Actor {
	a := stackrmw.WebActor(c)
	a.Via = ""
	return a
}

func (h *handler) load(c echo.Context) (*repo.Tile, error) {
	d, err := h.store.GetTile(c.Request().Context(), c.Param("id"))
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "database not found")
	}
	// The drawer has no breadcrumb, so its header says where the tile
	// lives. Resolved here because every panel path comes through load().
	components.StashTileLocation(c, h.store, d)
	return d, nil
}

// requireFileUnowned is RequireUnmanaged with the org exception: the file
// cannot declare an org-scoped instance (shared: stops at stack scope), so
// those stay panel-owned even on a config-managed stack. Scope changes are
// NOT exempt, moving an instance in or out of org scope on a managed stack
// makes it appear/vanish from the config snapshot mid-flight.
func (h *handler) requireFileUnowned(c echo.Context, d *repo.Tile) error {
	if d.ScopeKind == "org" {
		return nil
	}
	return stackrmw.RequireUnmanaged(c, h.store, d.StackID)
}

// GET /dbs/:id, the full page, hosting the same panel as the drawer.
func (h *handler) Detail(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	tab := c.QueryParam("tab")
	td, err := h.loadTab(c, d, tab)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, dbPage(c, d, tab, td))
}

// GET /dbs/:id/panel, the full right-drawer for this database (default tab).
func (h *handler) Panel(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	td, err := h.loadTab(c, d, "connection")
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, dbPanel(c, d, "connection", td, true))
}

// GET /dbs/:id/volume/panel, the drawer behind the tucked volume card on the
// graph. Read-only view of the docker volume holding this database's data.
func (h *handler) VolumePanel(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	// Only a database instance has a managed data volume; any other tile id
	// here would render a drawer describing a volume that doesn't exist.
	if !d.IsManaged() {
		return echo.NewHTTPError(http.StatusNotFound, "not a database")
	}
	return respond.HTML(c, http.StatusOK, volumePanel(d, h.dbs.DataVolume(c.Request().Context(), d), c.QueryParam("tab")))
}

// tabData carries per-tab payloads for the db panel.
type tabData struct {
	cpu, mem, rx, tx []components.TimePoint
	domains          []repo.Domain
	// fileOwned marks an instance the config file owns, so the settings form
	// says so instead of silently 409-ing on save. Org-scoped instances are
	// never file-owned (see requireFileUnowned).
	fileOwned bool
}

func (h *handler) loadTab(c echo.Context, d *repo.Tile, tab string) (tabData, error) {
	ctx := c.Request().Context()
	var td tabData
	var err error
	switch tab {
	case "logs":
		// live view, content arrives over SSE (LogsStream), nothing to preload
	case "metrics":
		_, dur := components.MetricRange(c.QueryParam("range"))
		td.cpu, td.mem, td.rx, td.tx = h.telemetry.Points(ctx, "db:"+d.ID, dur)
	case "settings":
		if td.domains, err = h.store.ListDomainsByTile(ctx, d.ID); err != nil {
			return td, err
		}
		if d.ScopeKind != "org" {
			if st, err := h.store.GetStack(ctx, d.StackID); err == nil && st != nil {
				td.fileOwned = st.ConfigManaged()
			}
		}
	default: // connection, the read-only overview
	}
	return td, nil
}

// GET /dbs/:id/panel/content?tab=..., swaps just the tabbed content region.
func (h *handler) PanelContent(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	tab := c.QueryParam("tab")
	td, err := h.loadTab(c, d, tab)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, dbPanelContent(c, d, tab, td))
}

// panelDone finishes a panel action: htmx requests get the tab re-rendered in
// place, plain requests fall back to the redirect. Without this an action taken
// inside the drawer navigates the whole page and closes the drawer the user was
// working in. Mirrors app.handler.panelDone.
func (h *handler) panelDone(c echo.Context, d *repo.Tile, tab string) error {
	if c.Request().Header.Get("HX-Request") != "true" {
		return respond.Redirect(c, "/dbs/"+d.ID+"?tab="+tab)
	}
	td, err := h.loadTab(c, d, tab)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, components.WithFlash(c, dbPanelContent(c, d, tab, td)))
}

// headerDone answers a status action: the header re-renders out of band and
// the tab body is left untouched, so the user keeps the tab they were on.
func (h *handler) headerDone(c echo.Context, d *repo.Tile) error {
	if c.Request().Header.Get("HX-Request") != "true" {
		return respond.Redirect(c, "/dbs/"+d.ID)
	}
	return respond.HTML(c, http.StatusOK, dbPanelHeaderOOB(c, d, true))
}

// GET /dbs/:id/logs/stream, SSE live-follow of the db container's logs.
func (h *handler) LogsStream(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/event-stream")
	res.Header().Set("Cache-Control", "no-cache")
	res.WriteHeader(http.StatusOK)
	// Service logs, not container logs: they survive swarm replacing the task
	// mid-stream, which a container id does not. A box that ran this instance
	// before the swarm move still has a plain container, hence the fallback.
	var (
		ch   <-chan string
		stop func()
	)
	if name := h.dbs.ServiceName(ctx, d); name != "" {
		ch, stop, err = h.clus.StreamServiceLogsMarked(ctx, name, 300)
	}
	if ch == nil {
		cs, _ := h.clus.ListByLabel(ctx, runtime.LabelDB, d.ID)
		if len(cs) == 0 {
			_, _ = fmt.Fprint(res, "data: O - no running container\n\n")
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

// GET /dbs/:id/metrics?range=1h|6h|24h, the charts fragment.
func (h *handler) Metrics(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	key, dur := components.MetricRange(c.QueryParam("range"))
	cpu, mem, rx, tx := h.telemetry.Points(c.Request().Context(), "db:"+d.ID, dur)
	return respond.HTML(c, http.StatusOK, components.MetricsFrag("/dbs/"+d.ID+"/metrics", key, cpu, mem, rx, tx))
}

// POST /dbs/:id/deploy
func (h *handler) Deploy(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	// Image pulls can take minutes; run detached so the request returns at once.
	go func() {
		if err := h.instances.Deploy(context.Background(), d); err != nil {
			slog.Error("database deploy failed", "tile", d.ID, "error", err)
		}
	}()
	middleware.SetFlash(c, "Database deploying. Refresh in a moment.", middleware.FlashSuccess)
	return h.headerDone(c, d)
}

// POST /dbs/:id/stop
func (h *handler) Stop(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	if err := h.life.Stop(c.Request().Context(), d); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Database stopped.", middleware.FlashSuccess)
	return h.headerDone(c, d)
}

// POST /dbs/:id/start
func (h *handler) Start(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	if err := h.life.Restart(c.Request().Context(), d); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Database starting. Refresh in a moment.", middleware.FlashSuccess)
	return h.headerDone(c, d)
}

// GET /dbs/:id/provisions, the provisioned-databases fragment.
func (h *handler) Provisions(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	return h.renderProvisions(c, d)
}

func (h *handler) renderProvisions(c echo.Context, d *repo.Tile) error {
	ctx := c.Request().Context()
	ps, err := h.store.ListProvisionsByInstance(ctx, d.ID)
	if err != nil {
		return err
	}
	// Group consumer rows into one entry per logical database.
	byDB := map[string]*dbGroup{}
	var order []string
	for _, p := range ps {
		g := byDB[p.DBName]
		if g == nil {
			// Public is per-bucket, so every row for one bucket agrees; the
			// first row is as good as any for the id the toggle posts back.
			g = &dbGroup{DBName: p.DBName, Public: p.Public, ProvisionID: p.ID,
				Slug: managedtiles.ResourceSlug(d, &p), Orphaned: p.Status == "orphaned"}
			byDB[p.DBName] = g
			order = append(order, p.DBName)
		}
		if p.ConsumerTileID != "" {
			if t, _ := h.store.GetTile(ctx, p.ConsumerTileID); t != nil {
				g.Consumers = append(g.Consumers, t.Name)
			}
		}
	}
	groups := make([]dbGroup, 0, len(order))
	for _, name := range order {
		groups = append(groups, *byDB[name])
	}
	return respond.HTML(c, http.StatusOK, dbProvisionsFrag(c, d, groups))
}

// POST /dbs/:id/provisions/drop, destroy a whole logical db (all consumers).
func (h *handler) DropProvision(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	dbName := c.FormValue("db_name")
	if err := h.slices.Drop(ctx, d, dbName); err != nil {
		middleware.SetFlash(c, "Drop failed: "+err.Error(), middleware.FlashError)
	} else {
		// The engine's own noun: this handler serves buckets as well as databases.
		middleware.SetFlash(c, strings.ToUpper(unitNoun(d.Engine)[:1])+unitNoun(d.Engine)[1:]+" "+dbName+" dropped.",
			middleware.FlashSuccess)
	}
	return h.renderProvisions(c, d)
}

// ForkProvision copies a slice's data into a new one on the same instance.
// POST /dbs/:id/provisions/fork
//
// Synchronous, like the drop beside it: the copy is an engine-level operation
// with no progress to report, and a fork that silently half-happened would be
// worse than a slow button.
func (h *handler) ForkProvision(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	src, err := h.store.GetProvision(ctx, c.FormValue("provision_id"))
	if err != nil || src == nil || src.InstanceTileID != d.ID {
		middleware.SetFlash(c, "Fork failed: no such "+unitNoun(d.Engine)+".", middleware.FlashError)
		return h.renderProvisions(c, d)
	}
	fork, err := h.slices.Fork(ctx, d, src, "", "")
	if err != nil {
		middleware.SetFlash(c, "Fork failed: "+err.Error(), middleware.FlashError)
	} else {
		middleware.SetFlash(c, "Forked "+src.DBName+" to "+fork.DBName+
			". No tile uses it yet.", middleware.FlashSuccess)
	}
	return h.renderProvisions(c, d)
}

// SetProvisionPublic flips one bucket's public-read policy.
// POST /dbs/:id/provisions/public
func (h *handler) SetProvisionPublic(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	// Resolved through this instance's own provisions, so a provision id from
	// another instance can't be steered in through the form.
	target, err := h.slices.Find(ctx, d, c.FormValue("provision_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "bucket not found on this instance")
	}
	public := c.FormValue("public") == "1"
	if err := h.slices.SetPublic(ctx, d, target, public); err != nil {
		middleware.SetFlash(c, "Could not change exposure: "+err.Error(), middleware.FlashError)
		return h.renderProvisions(c, d)
	}
	if public {
		middleware.SetFlash(c, "Bucket "+target.DBName+" is now publicly readable.", middleware.FlashSuccess)
	} else {
		middleware.SetFlash(c, "Bucket "+target.DBName+" is now private.", middleware.FlashSuccess)
	}
	return h.renderProvisions(c, d)
}

// POST /dbs/:id/delete
func (h *handler) Delete(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	if err := components.RequireConfirm(c, d.Slug); err != nil {
		return err
	}
	// force stays false from the panel: the drawer has no list of what would
	// be destroyed, so the refusal is the only thing that tells the user the
	// slices are there. The CLI prompts with the list and then forces.
	if err := h.instances.Delete(c.Request().Context(), d, false, h.actor(c)); err != nil {
		if conf, ok := svcerr.IsConflict(err); ok {
			middleware.SetFlash(c, conf.Msg, middleware.FlashError)
			return h.panelDone(c, d, "settings")
		}
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Database deleted. Its data volume was kept.", middleware.FlashSuccess)
	return respond.Redirect(c, "/projects/"+d.StackID)
}

// POST /dbs/:id/port, set external port + resource limits and redeploy.
func (h *handler) SetPort(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	// The form is the whole settings block, so every field is read back even
	// when it did not move; the service diffs the row it reads against the
	// stored one to decide whether the container has to be recreated.
	d.ExternalPort = formInt(c, "external_port")
	d.CPULimit = formFloat(c, "cpu_limit")
	d.MemLimitMB = formInt(c, "mem_limit_mb")
	d.ShmSizeMB = formInt(c, "shm_size_mb")
	d.ImageRef = strings.TrimSpace(c.FormValue("image_ref"))
	d.UpdatePolicy = c.FormValue("update_policy")
	if err := h.instances.Update(c.Request().Context(), d, h.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Settings updated.", middleware.FlashSuccess)
	return h.panelDone(c, d, "settings")
}

// formInt reads a whole-number field. An unparsable value becomes -1 rather
// than 0 so the service refuses it: the panel used to fold a typo into "not
// set", which silently unpublished a port or dropped a memory cap.
func formInt(c echo.Context, name string) int {
	raw := strings.TrimSpace(c.FormValue(name))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return -1
	}
	return n
}

// formFloat is formInt for a fractional field, with the same refusal.
func formFloat(c echo.Context, name string) float64 {
	raw := strings.TrimSpace(c.FormValue(name))
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return -1
	}
	return v
}

// POST /dbs/:id/scope, set how widely this instance is shared.
func (h *handler) SetScope(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	if err := h.instances.SetScope(c.Request().Context(), d, c.FormValue("scope_kind"), h.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Sharing scope updated to "+managedtiles.ScopeLabel(d.ScopeKind)+".", middleware.FlashSuccess)
	return h.panelDone(c, d, "settings")
}

// --- domains ---
//
// Only HTTP-speaking engines (s3/RustFS today) can sit behind Traefik; the
// form is hidden for the wire-protocol ones. The proxy layer itself is already
// tile-generic (proxy.WriteApp takes any tile) so this only adds the UI and
// the routing call.
//
// UI-managed stacks only. stackconf models domains for service tiles
// alone (syncDomains/diffDomains return early on tc.Type != "service"), so a
// db-tile domain on a config-managed stack would be invisible to plan/apply
// and would drift silently. Lift the restriction by teaching those two to
// handle db tiles.

// POST /dbs/:id/domains
func (h *handler) CreateDomain(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	if err := h.requireFileUnowned(c, d); err != nil {
		return err
	}
	https := c.FormValue("https") != ""
	// Port, the HTTP-engine refusal and the uniqueness rule are all in the
	// service now, so the API stopped accepting a managed id with port 0.
	_, _, err = h.domains.Attach(c.Request().Context(), d, service.DomainSpec{
		Host: c.FormValue("host"), Path: c.FormValue("path"),
		HTTPS: &https, ForceHTTPS: &https,
	}, h.actor(c))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Domain added.", middleware.FlashSuccess)
	return h.panelDone(c, d, "settings")
}

// POST /dbs/:id/domains/:domainID/delete
func (h *handler) DeleteDomain(c echo.Context) error {
	d, err := h.load(c)
	if err != nil {
		return err
	}
	if err := h.requireFileUnowned(c, d); err != nil {
		return err
	}
	if _, _, err := h.domains.Detach(c.Request().Context(), d, c.Param("domainID"), h.actor(c)); err != nil {
		return stackrmw.HTTP(err)
	}
	middleware.SetFlash(c, "Domain removed.", middleware.FlashSuccess)
	return h.panelDone(c, d, "settings")
}
