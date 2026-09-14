// Package server renders the servers screen: host stats history, Docker
// daemon info, and the server-level default settings.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/nodes"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/volmove"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store repo.Store
	rt    *runtime.Runtime
	px    *proxy.Proxy
	// cookieDomain is the host the panel's session cookie is scoped to (from
	// BASE_URL). Needed to tell the operator whether protecting preview URLs
	// would work or just lock everyone out.
	cookieDomain string

	// The swarm half (nodes.go). nodes keeps the table in step with the
	// swarm and issues join keys, clus is every docker call on any node, mover runs volume moves.
	nodes   *nodes.Service
	clus    *cluster.Cluster
	mover   *volmove.Service
	baseURL string
	dataDir string
	version string
}

// Deps is what the servers screens need. A struct rather than nine positional
// arguments, which is what it had grown to.
type Deps struct {
	Store        repo.Store
	Runtime      *runtime.Runtime
	Proxy        *proxy.Proxy
	CookieDomain string
	Nodes        *nodes.Service
	Cluster      *cluster.Cluster
	Mover        *volmove.Service
	BaseURL      string
	DataDir      string
	Version      string
}

// NewHandler creates a new server handler.
func NewHandler(d Deps) *handler {
	return &handler{
		store: d.Store, rt: d.Runtime, px: d.Proxy, cookieDomain: d.CookieDomain,
		nodes: d.Nodes, clus: d.Cluster, mover: d.Mover,
		baseURL: d.BaseURL, dataDir: d.DataDir, version: d.Version,
	}
}

// CookieCovers reports whether a browser holding the panel's session cookie
// would send it to a hostname under envDomainBase.
//
// A cookie set for "panel.example.com" is never sent to "app.preview.com", and
// is only sent to "x.panel.example.com" because that is a subdomain. So the
// forwardAuth on preview URLs is useful exactly when the panel's domain is the
// base itself or a parent of it.
//
// An unset base is "nothing to protect", which is not a misconfiguration. An
// unset cookie domain is: BASE_URL is empty, so the cookie is host-only for
// whatever hostname the panel was reached on and is never sent to a preview
// URL, the configuration where turning this on locks everyone out is exactly
// the one where the warning used to be hidden.
func CookieCovers(cookieDomain, envDomainBase string) bool {
	cookieDomain = strings.TrimPrefix(strings.TrimSpace(cookieDomain), ".")
	envDomainBase = strings.TrimSpace(envDomainBase)
	if envDomainBase == "" {
		return true
	}
	if cookieDomain == "" {
		return false
	}
	return envDomainBase == cookieDomain || strings.HasSuffix(envDomainBase, "."+cookieDomain)
}

// GET /servers/:id?range=..., stats history + docker info + settings.
func (h *handler) Detail(c echo.Context) error {
	ctx := c.Request().Context()
	sv, err := h.store.GetServer(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	if sv == nil {
		return echo.NewHTTPError(http.StatusNotFound, "server not found")
	}
	rangeKey, dur := metricRange(c.QueryParam("range"))
	cpu, mem, rx, tx := h.points(ctx, "server:"+sv.ID, dur)
	_, disk, _, _ := h.points(ctx, "server:"+sv.ID+":disk", dur)
	for i := range disk { // MB -> GB for a readable scale
		disk[i].V /= 1024
	}
	// Per node: the daemon info and the volumes both belong to whichever
	// machine this row is, and a volume is a directory on one host's disk.
	// The dispatcher answers from the socket for this node and through the
	// agent for any other.
	//
	// Not for a row that has never joined, though. A pending server has an
	// empty node_id, and agent.Nodes.IsSelf reads the empty string as
	// "wherever it is", which resolves to the manager, so this page used to
	// show the manager's docker version, core count, image count and whole
	// volume list as if they belonged to a machine that has no docker on it
	// at all, each volume with a live Delete button. "I do not mind which
	// node" and "this node does not exist yet" are the same value, so the
	// caller has to tell them apart.
	var info runtime.HostInfo
	var vols []runtime.VolumeInfo
	volMsg := ""
	if sv.NodeID != "" {
		nodeInfo, _ := h.clus.Info(ctx, sv.NodeID)
		info = nodeInfo.Host
		var volErr error
		vols, volErr = h.clus.ListVolumes(ctx, sv.NodeID)
		if volErr != nil {
			volMsg = volErr.Error()
		}
	}
	// Ping history is the manager's own measurement, so it works from the
	// day a node joins, with no agent involved.
	//
	// The first slot, not the second: nodes.Sampler writes the milliseconds
	// into Metric.CPUPct, and reading the mem slot instead charted the
	// mem_bytes column, which is zero on every ping row. The graph was a flat
	// line reading "0.0 ms" while the node table three rows up showed a real
	// figure from the live value.
	ping, _, _, _ := h.points(ctx, nodes.PingRef(sv.ID), dur)
	var node *runtime.Node
	if sv.NodeID != "" {
		if n, err := h.rt.GetNode(ctx, sv.NodeID); err == nil {
			node = &n
		}
	}
	groups, _ := h.knownGroups(ctx)
	allNodes, _ := h.rt.ListNodes(ctx) // the build-node picker
	var here []runtime.NodeTaskInfo
	if sv.NodeID != "" {
		here, _ = h.rt.NodeTasks(ctx, sv.NodeID)
	}
	// This server's own domain resources; the first one also feeds the
	// preview-URL cookie warning on the settings form.
	var domainRes []repo.DomainResource
	if res, err := h.store.ListDomainResources(ctx); err == nil {
		for _, r := range res {
			if r.Level == "instance" && r.OwnerID == sv.ID {
				domainRes = append(domainRes, r)
			}
		}
	}
	instanceBase := ""
	if len(domainRes) > 0 {
		instanceBase = domainRes[0].Host
	}
	// This server's own, not every server's: each one is a directory on this
	// machine, and the page's Add form writes this server's id.
	all, _ := h.store.ListStorage(ctx)
	storages := make([]repo.Storage, 0, len(all))
	spaths := map[string][]repo.StoragePath{}
	for i := range all {
		if all[i].ServerID != sv.ID {
			continue
		}
		storages = append(storages, all[i])
		ps, _ := h.store.ListStoragePaths(ctx, all[i].ID)
		spaths[all[i].ID] = ps
	}
	return respond.HTML(c, http.StatusOK, serverPage(c, sv, node, here, groups, ping, volMsg,
		h.cookieDomain, instanceBase, domainRes, rangeKey, cpu, mem, disk, rx, tx, info, vols, storages, spaths, allNodes))
}

// knownGroups is every group already set on a node, so the Group field can
// suggest them rather than asking the operator to remember how they spelled
// it last time.
func (h *handler) knownGroups(ctx context.Context) ([]string, error) {
	ns, err := h.rt.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range ns {
		if n.Group != "" && !seen[n.Group] {
			seen[n.Group] = true
			out = append(out, n.Group)
		}
	}
	return out, nil
}

// POST /servers/:id/domains, add an instance-level domain resource.
func (h *handler) CreateDomainResource(c echo.Context) error {
	ctx := c.Request().Context()
	sv, err := h.store.GetServer(ctx, c.Param("id"))
	if err != nil || sv == nil {
		return echo.NewHTTPError(http.StatusNotFound, "server not found")
	}
	host := strings.TrimSpace(c.FormValue("host"))
	if err := envops.ValidateResourceHost(host); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	all, err := h.store.ListDomainResources(ctx)
	if err != nil {
		return err
	}
	if envops.HostTaken(all, host) {
		return echo.NewHTTPError(http.StatusConflict, "that host is already a domain resource")
	}
	r := &repo.DomainResource{
		ID: uuid.New().String(), Level: "instance", OwnerID: sv.ID, Host: host,
		IncludeEnvOnDefault: c.FormValue("include_env_on_default") != "",
		CreatedAt:           time.Now().UTC(),
	}
	if err := h.store.CreateDomainResource(ctx, r); err != nil {
		return err
	}
	middleware.SetFlash(c, "Domain "+host+" added. Tiles can now claim auto hostnames under it.", middleware.FlashSuccess)
	return respond.Redirect(c, "/servers/"+sv.ID)
}

// POST /servers/:id/domains/delete
func (h *handler) DeleteDomainResource(c echo.Context) error {
	if err := h.store.DeleteDomainResource(c.Request().Context(), c.FormValue("id")); err != nil {
		return err
	}
	middleware.SetFlash(c, "Domain resource removed. Existing generated hostnames keep working until their tile redeploys.", middleware.FlashSuccess)
	return respond.Redirect(c, "/servers/"+c.Param("id"))
}

// volumeNode resolves the :id in a volume route to the node that owns the
// volumes on that page. A volume is a directory on one host's disk, so these
// routes are meaningless without it: they used to ignore :id entirely and run
// against the manager, which deleted the manager's volume of that name while
// the operator was looking at a worker's list.
//
// A server with no node_id has not joined the swarm yet. That is not the same
// as "wherever it is", the dispatcher reads an empty node ID as the local
// machine, so it is refused here rather than being allowed to collapse onto
// the manager.
func (h *handler) volumeNode(c echo.Context) (string, error) {
	sv, err := h.store.GetServer(c.Request().Context(), c.Param("id"))
	if err != nil {
		return "", err
	}
	if sv == nil {
		return "", echo.NewHTTPError(http.StatusNotFound, "server not found")
	}
	if sv.NodeID == "" {
		return "", echo.NewHTTPError(http.StatusConflict,
			sv.Name+" has not joined yet, so it has no volumes of its own. Run its join script first.")
	}
	return sv.NodeID, nil
}

// POST /servers/:id/volumes, create a named docker volume on that node.
func (h *handler) CreateVolume(c echo.Context) error {
	name := c.FormValue("name")
	if !repo.ValidVolumeName(name) {
		return echo.NewHTTPError(http.StatusBadRequest, "volume name: letters, digits, _ . - only")
	}
	node, err := h.volumeNode(c)
	if err != nil {
		return err
	}
	if err := h.clus.CreateVolume(c.Request().Context(), node, name); err != nil {
		return err
	}
	middleware.SetFlash(c, "Volume "+name+" created. Mount it from a tile's settings as "+name+":/path.", middleware.FlashSuccess)
	return respond.Redirect(c, "/servers/"+c.Param("id"))
}

// POST /servers/:id/volumes/delete, remove an unused volume from that node.
func (h *handler) DeleteVolume(c echo.Context) error {
	name := c.FormValue("name")
	if !repo.ValidVolumeName(name) {
		return echo.NewHTTPError(http.StatusBadRequest, "volume name: letters, digits, _ . - only")
	}
	node, err := h.volumeNode(c)
	if err != nil {
		return err
	}
	// A volume is the one thing here with no copy anywhere: removing it is the
	// data, gone. Typing the name is the guard, but it has to be checked
	// against what the node actually has: confirming the posted name against
	// the posted name is satisfied by any two matching fields. Confirmed
	// refuses an empty expectation, so a name this node does not have fails
	// closed.
	want := ""
	if vols, err := h.clus.ListVolumes(c.Request().Context(), node); err == nil {
		for _, v := range vols {
			if v.Name == name {
				want = v.Name
			}
		}
	}
	if err := components.RequireConfirm(c, want); err != nil {
		return err
	}
	if err := h.clus.RemoveVolume(c.Request().Context(), node, name); err != nil {
		middleware.SetFlash(c, "Delete failed: "+err.Error(), middleware.FlashError)
	} else {
		middleware.SetFlash(c, "Volume deleted.", middleware.FlashSuccess)
	}
	return respond.Redirect(c, "/servers/"+c.Param("id"))
}

// POST /servers/:id/settings, save the default-settings blob.
func (h *handler) SaveSettings(c echo.Context) error {
	ctx := c.Request().Context()
	sv, err := h.store.GetServer(ctx, c.Param("id"))
	if err != nil || sv == nil {
		return echo.NewHTTPError(http.StatusNotFound, "server not found")
	}
	vals, err := c.FormParams()
	if err != nil {
		return err
	}
	sv.Name = c.FormValue("name")
	sv.Settings = settings.Merge(settings.Parse(sv.Settings), vals).JSON()
	if err := h.store.UpdateServer(ctx, sv); err != nil {
		return err
	}
	// Settings feed the rendered proxy routes (protect_auto_domains), which are
	// otherwise only rewritten on domain changes, a toggle here must take
	// effect now, not on the next deploy.
	if h.px != nil {
		if err := h.px.Resync(ctx); err != nil {
			slog.Error("proxy resync after settings save", "error", err)
		}
	}
	middleware.SetFlash(c, "Server settings saved.", middleware.FlashSuccess)
	return respond.Redirect(c, "/servers/"+sv.ID)
}

func metricRange(key string) (string, time.Duration) {
	switch key {
	case "6h":
		return "6h", 6 * time.Hour
	case "24h":
		return "24h", 24 * time.Hour
	}
	return "1h", time.Hour
}

// points loads bucket-averaged samples (same shape as the app charts).
func (h *handler) points(ctx context.Context, ref string, dur time.Duration) (cpu, mem, rx, tx []components.TimePoint) {
	ms, _ := h.store.ListMetrics(ctx, ref, time.Now().Add(-dur))
	step := len(ms)/240 + 1
	for i := 0; i < len(ms); i += step {
		end := i + step
		if end > len(ms) {
			end = len(ms)
		}
		var cv, mv, rv, tv float64
		for _, s := range ms[i:end] {
			cv += s.CPUPct
			mv += float64(s.MemBytes) / (1024 * 1024)
			rv += s.RxBps / 1024
			tv += s.TxBps / 1024
		}
		n := float64(end - i)
		cpu = append(cpu, components.TimePoint{T: ms[i].TS, V: cv / n})
		mem = append(mem, components.TimePoint{T: ms[i].TS, V: mv / n})
		rx = append(rx, components.TimePoint{T: ms[i].TS, V: rv / n})
		tx = append(tx, components.TimePoint{T: ms[i].TS, V: tv / n})
	}
	return
}
