package deployment

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/stream"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// loadTile fetches an org-checked tile by the :id param.
func (h *handler) loadTile(c echo.Context) (*repo.Tile, error) {
	app, err := h.store.GetTile(c.Request().Context(), c.Param("id"))
	if err != nil {
		return nil, err
	}
	if app == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "app not found")
	}
	if err := stackrmw.RequireStackAccess(c, h.store, app.StackID); err != nil {
		return nil, err
	}
	return app, nil
}

// loadDeployment fetches an org-checked deployment (and its tile) by :id.
func (h *handler) loadDeployment(c echo.Context) (*repo.Deployment, *repo.Tile, error) {
	d, err := h.store.GetDeployment(c.Request().Context(), c.Param("id"))
	if err != nil {
		return nil, nil, err
	}
	if d == nil {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "deployment not found")
	}
	app, err := h.store.GetTile(c.Request().Context(), d.TileID)
	if err != nil {
		return nil, nil, err
	}
	if app != nil {
		if err := stackrmw.RequireStackAccess(c, h.store, app.StackID); err != nil {
			return nil, nil, err
		}
	}
	return d, app, nil
}

type handler struct {
	store  repo.Store
	engine *deploy.Engine
	hub    *stream.Hub
}

// NewHandler creates a new deployment handler.
func NewHandler(store repo.Store, engine *deploy.Engine, hub *stream.Hub) *handler {
	return &handler{store: store, engine: engine, hub: hub}
}

// upperEnvDeploy is why a tile above the default env has no Deploy: its
// images come from Promote on the releases page.
const upperEnvDeploy = "this environment deploys by promote from the releases page"

// POST /apps/:id/deploy
func (h *handler) Deploy(c echo.Context) error {
	app, err := h.loadTile(c)
	if err != nil {
		return err
	}
	if app.Kind == "cron" {
		return echo.NewHTTPError(http.StatusBadRequest, "cron services run on their schedule; use Run now")
	}
	if envnet.UpperEnv(c.Request().Context(), h.store, app) {
		return echo.NewHTTPError(http.StatusBadRequest, upperEnvDeploy)
	}
	id, err := h.engine.Enqueue(c.Request().Context(), app, "manual")
	if err != nil {
		return err
	}
	return respond.Redirect(c, "/deployments/"+id)
}

// POST /apps/:id/rollback
func (h *handler) Rollback(c echo.Context) error {
	app, err := h.loadTile(c)
	if err != nil {
		return err
	}
	tag := c.FormValue("image_tag")
	if tag == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "image tag required")
	}
	id, err := h.engine.EnqueueRollback(c.Request().Context(), app, tag)
	if err != nil {
		return err
	}
	return respond.Redirect(c, "/deployments/"+id)
}

// GET /deployments/:id
func (h *handler) Detail(c echo.Context) error {
	d, app, err := h.loadDeployment(c)
	if err != nil {
		return err
	}
	return respond.HTML(c, http.StatusOK, deploymentPage(c, d, app))
}

// GET /deployments/:id/status, polled status badge partial.
func (h *handler) Status(c echo.Context) error {
	d, _, err := h.loadDeployment(c)
	if err != nil {
		return err
	}
	if d.Status != "queued" && d.Status != "running" {
		// Final state: stop the polling swap loop.
		c.Response().Header().Set("HX-Reswap", "outerHTML")
	}
	return respond.HTML(c, http.StatusOK, statusFragment(c, d))
}

// POST /deployments/:id/cancel
func (h *handler) Cancel(c echo.Context) error {
	if _, _, err := h.loadDeployment(c); err != nil {
		return err
	}
	h.engine.Cancel(c.Request().Context(), c.Param("id"))
	return respond.Redirect(c, "/deployments/"+c.Param("id"))
}

// frameLog writes build output as SSE lines in the "O <ts> <msg>" format the
// viewer parses, one event per line, the viewer renders a row per event, so a
// single event holding many newlines would collapse into one row.
func frameLog(w io.Writer, data string, ts time.Time) {
	stamp := ts.UTC().Format(time.RFC3339)
	for _, line := range strings.Split(strings.TrimRight(data, "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		// buildkit separates its steps with blank lines. In a <pre> those read
		// as spacing; as viewer rows they are a timestamp and nothing else,
		// and they were a third of the page.
		if line == "" {
			continue
		}
		_, _ = fmt.Fprintf(w, "data: O %s %s\n\n", stamp, line)
	}
}

// GET /deployments/:id/stream, SSE: replay the log file, then live-follow
// until the deployment reaches a final state.
func (h *handler) Stream(c echo.Context) error {
	ctx := c.Request().Context()
	d, _, err := h.loadDeployment(c)
	if err != nil {
		return err
	}

	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/event-stream")
	res.Header().Set("Cache-Control", "no-cache")
	res.WriteHeader(http.StatusOK)

	// The viewer parses "O <ts> <msg>" (what container streams emit), so the
	// build log is framed the same way here rather than in the log file, the
	// file is served raw to the API and CLI. One SSE event per line: the
	// viewer renders a row per event.
	send := func(data string, ts time.Time) {
		frameLog(res, data, ts)
		res.Flush()
	}

	// Subscribe before replay so no lines are lost; a line landing during
	// replay may appear twice. duplicate log lines beat dropped ones.
	logCh, unsubLog := h.hub.Subscribe("deploy:" + d.ID)
	defer unsubLog()
	statusCh, unsubStatus := h.hub.Subscribe("deploy-status:" + d.ID)
	defer unsubStatus()

	// Replayed lines have no time of their own; the build's start is the
	// closest true thing.
	started := d.CreatedAt
	if d.StartedAt.Valid {
		started = d.StartedAt.Time
	}
	if b, err := os.ReadFile(h.engine.LogPath(d.ID)); err == nil && len(b) > 0 {
		send(string(b), started)
	}

	// Already finished? One event and done.
	d, _ = h.store.GetDeployment(ctx, d.ID)
	if d != nil && d.Status != "queued" && d.Status != "running" {
		_, _ = fmt.Fprintf(res, "event: done\ndata: %s\n\n", d.Status)
		res.Flush()
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-logCh:
			send(msg, time.Now())
		case st := <-statusCh:
			if st != "running" {
				_, _ = fmt.Fprintf(res, "event: done\ndata: %s\n\n", st)
				res.Flush()
				return nil
			}
		}
	}
}
