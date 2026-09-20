package container

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
)

type handler struct {
	// clus is every docker call. This page is the one place in the panel that
	// shows raw containers, so on a swarm it has to show every node's; the
	// manager's own socket knows about its own and nothing else.
	clus     *cluster.Cluster
	notifier *notify.Notifier
	// containers owns the system-container guard the four verbs share.
	containers *service.ContainerService
}

// NewHandler creates a new container browser handler.
func NewHandler(clus *cluster.Cluster, notifier *notify.Notifier, containers *service.ContainerService) *handler {
	return &handler{clus: clus, notifier: notifier, containers: containers}
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
	Node string
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
		return respond.HTML(c, http.StatusOK, listPage(c, rows, nil, showStopped, stopped))
	}
	// Sections only: each node's list is its own request, NodeList, so a node
	// that does not answer delays its own section and not the page.
	return respond.HTML(c, http.StatusOK, listPage(c, nil, nodes, showStopped, 0))
}

// GET /containers/node/:id?host=, one node's section of the list.
//
// The hostname travels in the query so this is one agent call and no node
// lookup: a container event refreshes every section at once. A node that has
// left the swarm fails the call and shows as not answering until the page's
// own poll re-reads the node list and drops the section.
//
// A failed node is rendered into the section, not flashed. middleware.SetFlash
// writes a cookie the *next* request reads, so a flash set here would never be
// seen. And it is a notice rather than a toast because it is not an event: the
// node is still unreachable while you read the page.
func (h *handler) NodeList(c echo.Context) error {
	// Bounded: a hung agent otherwise holds the section until the request
	// timeout turns it into a 500 and the notice never shows.
	ctx, cancel := components.PageCtx(c.Request().Context())
	defer cancel()
	showStopped := c.QueryParam("stopped") == "1"
	n := runtime.Node{ID: c.Param("id"), Hostname: c.QueryParam("host")}
	cs, err := h.clus.ListAll(ctx, n.ID)
	if err != nil {
		return respond.HTML(c, http.StatusOK, nodeSection(c, n, nil, true, showStopped, 0))
	}
	rows := make([]ctRow, len(cs))
	for i, ct := range cs {
		rows[i] = ctRow{ManagedContainer: ct, Node: n.ID}
	}
	rows, stopped := applyFilter(rows, showStopped)
	return respond.HTML(c, http.StatusOK, nodeSection(c, n, rows, false, showStopped, stopped))
}

// GET /containers/:id
func (h *handler) Detail(c echo.Context) error {
	ctx, cancel := components.PageCtx(c.Request().Context())
	defer cancel()
	d, down, err := h.inspect(ctx, c)
	if err != nil {
		return err
	}
	var st runtime.ContainerStats
	if d.State == "running" {
		st, _ = h.clus.Stats(ctx, h.node(c), d.ID) // zero-value on error
	}
	// The node travels with the id here too. Without it this page's own
	// "View logs" and "Open terminal" links 404 for every container on a
	// worker, while the same two links on the list page work, the list keeps
	// the node and this screen dropped it.
	return respond.HTML(c, http.StatusOK, detailPage(c, d, st, h.node(c), down))
}

// inspect is the container a page is about, under the page's deadline. Docker
// saying the container does not exist is a 404. A node that does not answer
// (a halted node fails at the dial, a hung one at the deadline) renders the
// page on the short id with down set, because the container is most likely
// still there. Everything else, a bad node id, a stale agent, a bad key, is
// an error the operator has to see.
func (h *handler) inspect(ctx context.Context, c echo.Context) (*runtime.ContainerDetail, bool, error) {
	id := c.Param("id")
	d, err := h.clus.InspectContainer(ctx, h.node(c), id)
	if err == nil {
		return d, false, nil
	}
	// The agent flattens docker's error to its message, so the message is
	// the one thing both paths share.
	if strings.Contains(err.Error(), "No such container") {
		return nil, false, echo.NewHTTPError(http.StatusNotFound, "container not found")
	}
	var ne net.Error
	if !errors.Is(err, context.DeadlineExceeded) && !errors.As(err, &ne) {
		return nil, false, err
	}
	slog.Warn("container page: node not answering", "node", h.node(c), "container", id, "error", err)
	name := id
	if len(name) > 12 {
		name = name[:12]
	}
	return &runtime.ContainerDetail{ID: id, Name: name}, true, nil
}

// backToList is the list, keeping the stopped filter the action was started
// from. Removing a corpse otherwise bounces the operator back to the filtered
// view they had just opened to reach it.
func backToList(c echo.Context) string {
	return listURL(c.QueryParam("stopped") == "1")
}

// POST /containers/:id/start | stop | remove
//
// The system-container guard is ContainerService's, one rule for all three.
func (h *handler) Start(c echo.Context) error {
	return h.containerAction(c, h.containers.Start)
}

func (h *handler) Stop(c echo.Context) error {
	return h.containerAction(c, h.containers.Stop)
}

func (h *handler) Remove(c echo.Context) error {
	return h.containerAction(c, h.containers.Remove)
}

func (h *handler) containerAction(c echo.Context, do func(context.Context, string, string) error) error {
	if err := do(c.Request().Context(), h.node(c), c.Param("id")); err != nil {
		return stackrmw.HTTP(err)
	}
	h.notifier.Containers()
	return respond.Redirect(c, backToList(c))
}

// GET /containers/:id/logs, page with live SSE view.
func (h *handler) LogsPage(c echo.Context) error {
	ctx, cancel := components.PageCtx(c.Request().Context())
	defer cancel()
	d, down, err := h.inspect(ctx, c)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, logsPage(c, d, h.node(c), down))
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
