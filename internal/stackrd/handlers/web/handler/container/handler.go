package container

import (
	"fmt"
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
)

type handler struct {
	// clus is every docker call. This page is the one place in the panel that
	// shows raw containers, so on a swarm it has to show every node's; the
	// manager's own socket knows about its own and nothing else.
	clus     *cluster.Cluster
	notifier *notify.Notifier
}

// NewHandler creates a new container browser handler.
func NewHandler(clus *cluster.Cluster, notifier *notify.Notifier) *handler {
	return &handler{clus: clus, notifier: notifier}
}

// node is the node a request is about. Every route carries it as ?node=,
// written by the list page. Absent means this machine, and that is said
// here, once, as the panel's own node id: the cluster refuses an empty node
// rather than guessing (docs/plans/35-cluster.md).
func (h *handler) node(c echo.Context) string {
	if n := c.QueryParam("node"); n != "" {
		return n
	}
	return h.clus.Self(c.Request().Context())
}

// row is one container and the node it is on. The node has to travel with
// the id: a container id is unique to its own daemon, and every action below
// has to be sent to the daemon that has it.
type ctRow struct {
	runtime.ManagedContainer
	Node     string
	NodeName string
	// ShowStopped is the list filter this row was rendered under, so that an
	// action taken on it returns to the same view rather than the default one.
	ShowStopped bool
}

// path is a route for this container, carrying its node and the list filter.
func (r ctRow) path(suffix string) string {
	p := "/containers/" + r.ID + suffix
	sep := "?"
	if r.Node != "" {
		p += sep + "node=" + r.Node
		sep = "&"
	}
	if r.ShowStopped {
		p += sep + "stopped=1"
	}
	return p
}

// applyFilter drops the rows that are not running unless asked for, and
// reports how many there were either way. Swarm keeps several generations of
// every task, so on a box deployed more than a couple of times the corpses
// outnumber the live containers and bury the row an operator came looking
// for. The count is always rendered, because an
// exited agent or proxy row is the only place its remove button lives.
func applyFilter(rows []ctRow, showStopped bool) ([]ctRow, int) {
	out := make([]ctRow, 0, len(rows))
	stopped := 0
	for _, r := range rows {
		if r.State != "running" {
			stopped++
			if !showStopped {
				continue
			}
		}
		r.ShowStopped = showStopped
		out = append(out, r)
	}
	return out, stopped
}

// GET /containers, ?stopped=1 to include containers that are not running.
func (h *handler) List(c echo.Context) error {
	ctx := c.Request().Context()
	showStopped := c.QueryParam("stopped") == "1"
	nodes, err := h.clus.ListNodes(ctx)
	if err != nil || len(nodes) < 2 {
		// Not a swarm, or a swarm of one: the local socket is the whole
		// answer and no node has to travel with anything.
		cs, err := h.clus.ListAll(ctx, h.clus.Self(ctx))
		if err != nil {
			return err
		}
		rows := make([]ctRow, len(cs))
		for i, ct := range cs {
			rows[i] = ctRow{ManagedContainer: ct}
		}
		rows, stopped := applyFilter(rows, showStopped)
		return respond.HTML(c, http.StatusOK, listPage(c, rows, false, nil, showStopped, stopped))
	}
	var rows []ctRow
	var failed []string
	for _, n := range nodes {
		cs, err := h.clus.ListAll(ctx, n.ID)
		if err != nil {
			// One unreachable node must not blank the whole page. Its name
			// is reported instead, so "missing" is never silent.
			failed = append(failed, n.Hostname)
			continue
		}
		for _, ct := range cs {
			rows = append(rows, ctRow{ManagedContainer: ct, Node: n.ID, NodeName: n.Hostname})
		}
	}
	// Rendered into the page, not flashed. middleware.SetFlash writes a cookie
	// the *next* request reads, so a flash set here and rendered here is never
	// seen, the operator got a page with a node's containers silently missing
	// and no word about it.
	//
	// A banner rather than a toast, because this is not an event: the node is
	// still unreachable while you read the page, and the notice should last as
	// long as the condition does.
	rows, stopped := applyFilter(rows, showStopped)
	return respond.HTML(c, http.StatusOK, listPage(c, rows, true, failed, showStopped, stopped))
}

// GET /containers/:id
func (h *handler) Detail(c echo.Context) error {
	ctx := c.Request().Context()
	d, err := h.clus.InspectContainer(ctx, h.node(c), c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "container not found")
	}
	var st runtime.ContainerStats
	if d.State == "running" {
		st, _ = h.clus.Stats(ctx, h.node(c), d.ID) // zero-value on error
	}
	// The node travels with the id here too. Without it this page's own
	// "View logs" and "Open terminal" links 404 for every container on a
	// worker, while the same two links on the list page work, the list keeps
	// the node and this screen dropped it.
	return respond.HTML(c, http.StatusOK, detailPage(c, d, st, h.node(c)))
}

// backToList is the list, keeping the stopped filter the action was started
// from. Removing a corpse otherwise bounces the operator back to the filtered
// view they had just opened to reach it.
func backToList(c echo.Context) string {
	return listURL(c.QueryParam("stopped") == "1")
}

// POST /containers/:id/start | stop | remove
func (h *handler) Start(c echo.Context) error {
	if err := h.clus.StartContainer(c.Request().Context(), h.node(c), c.Param("id")); err != nil {
		return err
	}
	h.notifier.Containers()
	return respond.Redirect(c, backToList(c))
}

func (h *handler) Stop(c echo.Context) error {
	ctx := c.Request().Context()
	if h.clus.ContainerIsSystem(ctx, h.node(c), c.Param("id")) {
		return echo.NewHTTPError(http.StatusForbidden, "the panel and proxy containers can't be stopped from here")
	}
	if err := h.clus.StopContainer(ctx, h.node(c), c.Param("id")); err != nil {
		return err
	}
	h.notifier.Containers()
	return respond.Redirect(c, backToList(c))
}

func (h *handler) Remove(c echo.Context) error {
	ctx := c.Request().Context()
	if h.clus.ContainerIsSystem(ctx, h.node(c), c.Param("id")) {
		// A system container that has already exited is a previous generation
		// left behind by an upgrade. It cuts nothing off, and refusing it is
		// what left every node accumulating agent rows no operator could ever
		// clear. The guard is about the running
		// one, which is the one the panel depends on.
		d, err := h.clus.InspectContainer(ctx, h.node(c), c.Param("id"))
		if err != nil || d.State == "running" {
			return echo.NewHTTPError(http.StatusForbidden,
				"the panel, proxy and node agent containers cannot be removed while they are running")
		}
	}
	if err := h.clus.StopRemove(ctx, h.node(c), c.Param("id")); err != nil {
		return err
	}
	h.notifier.Containers()
	return respond.Redirect(c, backToList(c))
}

// GET /containers/:id/logs, page with live SSE view.
func (h *handler) LogsPage(c echo.Context) error {
	d, err := h.clus.InspectContainer(c.Request().Context(), h.node(c), c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "container not found")
	}
	return respond.HTML(c, http.StatusOK, logsPage(c, d, h.node(c)))
}

// GET /containers/:id/logs/stream, SSE follow.
func (h *handler) LogsStream(c echo.Context) error {
	ctx := c.Request().Context()
	// Marked stream ("O <ts> <msg>" / "E <ts> <msg>"), the wire format the
	// shared components.LogView viewer parses.
	ch, stop, err := h.clus.StreamLogsMarked(ctx, h.node(c), c.Param("id"), 300)
	if err != nil {
		return err
	}
	defer stop()

	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/event-stream")
	res.Header().Set("Cache-Control", "no-cache")
	res.WriteHeader(http.StatusOK)

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
