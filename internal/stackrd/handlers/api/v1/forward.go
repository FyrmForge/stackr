package v1

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/wsproxy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/forward"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// ForwardPortHeader carries the resolved container port back on the upgrade
// response, so a client that asked for the default learns what it got.
const ForwardPortHeader = "X-Stackr-Port"

// forwardTile tunnels a websocket to a TCP port inside the tile's container,
// the server half of `stackr forward`, kubectl-port-forward style. The client
// opens one of these per local connection it accepts.
//
// Deliberately not restricted to databases: an app's HTTP port is as
// forwardable as postgres, and the code is identical. Note what this bypasses,
// Traefik middleware (basic auth, sec headers, TLS) never sees this traffic,
// so the tiles:forward scope is the only gate.
func (a *API) forwardTile(c echo.Context) error {
	// write, not read: a tunnel is unrestricted access to the container, so
	// this matches ScopeTilesForward being declared a write capability. A
	// read-only org member must not be able to open one.
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	port, err := forwardPort(t, c.QueryParam("port"))
	if err != nil {
		return err
	}

	ctx := c.Request().Context()
	// Swarm task state, not a label lookup on the local socket: a tile
	// running on a worker has no container here, and the old lookup called
	// that "tile is not running".
	tasks, err := a.rt.RunningTasks(ctx, envnet.ServiceFor(ctx, a.store, t))
	if err != nil || len(tasks) == 0 {
		return echo.NewHTTPError(http.StatusConflict, "tile is not running")
	}
	// The env network, from the environment row rather than from inspecting
	// a container this machine may not have. It is an overlay, so the tile's
	// alias resolves on it from the manager whichever node the task is on.
	//
	// first task wins, matching what logs does. A replica picker
	// lands with the task resolver (docs/plans/30-docker-swarm.md).
	envNet, err := envnet.Net(ctx, a.store, t.EnvironmentID)
	if err != nil || envNet == "" {
		return echo.NewHTTPError(http.StatusConflict, "tile is not on a network yet; deploy it first")
	}
	conn, err := a.rt.DialOnNetwork(ctx, t.ID, envNet, envnet.TileAlias(t.ID), port)
	if err != nil {
		// Dial before the upgrade so this is a status the CLI can print,
		// after Accept there is no way to report anything but a close.
		return echo.NewHTTPError(http.StatusBadGateway, "cannot reach "+t.Slug+":"+strconv.Itoa(port)+" ("+err.Error()+")")
	}

	// the repo has no audit trail, so a log line is the record of
	// who tunnelled where. Promote to a real table if one ever lands.
	var userID, userName, userAvatar, userRole string
	if u := a.user(c); u != nil {
		userID, userName = u.ID, u.Name
		userAvatar, userRole = u.AvatarPath, u.Role
	}
	// The client sends no port when it wants the tile's own; tell it what that
	// resolved to so it can name the local end and print an honest line. Set
	// before Accept, the upgrade response is the only one it will get.
	c.Response().Header().Set(ForwardPortHeader, strconv.Itoa(port))

	// The CLI's lifetime websocket: registers who has a forward open (the
	// canvas card keys off this, not off per-request data tunnels) and doubles
	// as its startup probe, the dial above already proved the target
	// reachable, so the connection itself is surplus.
	if c.QueryParam("presence") == "1" {
		_ = conn.Close()
		return a.forwardPresence(c, t, port, forward.Session{
			TileID: t.ID, Port: port,
			UserID: userID, UserName: userName, AvatarPath: userAvatar, Role: userRole,
		})
	}

	started := time.Now()
	slog.Info("port-forward opened", "user", userID, "tile", t.Slug, "tile_id", t.ID, "port", port)
	defer func() {
		slog.Info("port-forward closed", "user", userID, "tile", t.Slug, "tile_id", t.ID, "port", port, "duration", time.Since(started).Round(time.Second))
	}()
	return wsproxy.Serve(c, conn)
}

// forwardPresence parks the CLI's session websocket until the process ends,
// the row on the canvas card exists exactly as long as this does. Data moves
// through separate per-connection tunnels; this one carries nothing.
func (a *API) forwardPresence(c echo.Context, t *repo.Tile, port int, sess forward.Session) error {
	ctx := c.Request().Context()
	// Resolve the stack up front: the deferred nudge runs after the websocket
	// dies, when the request context is already cancelled.
	var stackID string
	if env, err := a.store.GetEnvironment(ctx, t.EnvironmentID); err == nil && env != nil {
		stackID = env.StackID
	}
	nudge := func() {
		if a.notify != nil && stackID != "" {
			// notify.Project is the websocket room name, shared with the web
			// layer's JS, it keeps the old word until that layer is renamed too.
			a.notify.Project(stackID)
		}
	}

	ws, err := websocket.Accept(c.Response(), c.Request(), nil)
	if err != nil {
		return nil // Accept already wrote the error response
	}
	defer func() { _ = ws.CloseNow() }()

	sess.StartedAt = time.Now()
	slog.Info("port-forward session opened", "user", sess.UserID, "tile", t.Slug, "tile_id", t.ID, "port", port)
	remove := func() {}
	if a.fwd != nil {
		remove = a.fwd.Add(sess)
	}
	nudge()
	defer func() {
		slog.Info("port-forward session closed", "user", sess.UserID, "tile", t.Slug, "tile_id", t.ID, "port", port, "duration", time.Since(sess.StartedAt).Round(time.Second))
		remove()
		nudge()
	}()

	// Block until the CLI goes away. WithoutCancel dodges the server's 30s
	// request ContextTimeout, same as wsproxy.
	//
	// A presence session that outlives its CLI is a user who shows on the
	// canvas forever and a relay whose idle reaper never fires, so the socket
	// needs something that notices a client which died without a FIN. Nothing
	// else does: coder/websocket sends no keepalives of its own, and this end
	// only ever reads.
	sessCtx := context.WithoutCancel(ctx)
	go pingLoop(sessCtx, ws)
	wc := websocket.NetConn(sessCtx, ws, websocket.MessageBinary)
	_, _ = io.Copy(io.Discard, wc)
	return nil
}

// forwardPingEvery is how often a presence socket is probed. Well under the
// idle timeout of anything likely to sit in front of the panel, and cheap:
// one frame per open forward per interval.
const forwardPingEvery = 30 * time.Second

// pingLoop closes the socket once a ping goes unanswered, a half-open TCP
// connection (laptop closed, VPN dropped) blocks reads forever otherwise.
func pingLoop(ctx context.Context, ws *websocket.Conn) {
	t := time.NewTicker(forwardPingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pingCtx, cancel := context.WithTimeout(ctx, forwardPingEvery)
			err := ws.Ping(pingCtx)
			cancel()
			if err != nil {
				_ = ws.CloseNow() // unblocks the read, which tears the session down
				return
			}
		}
	}
}

// forwardPort resolves the container-side port: the explicit query value, else
// the engine's default for a database, else the tile's declared port.
func forwardPort(t *repo.Tile, q string) (int, error) {
	if q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 || n > 65535 {
			return 0, echo.NewHTTPError(http.StatusBadRequest, "invalid port")
		}
		return n, nil
	}
	if t.IsManaged() {
		if e, ok := managedtiles.Engines[t.Engine]; ok && e.Port > 0 {
			return e.Port, nil
		}
	}
	if t.ContainerPort > 0 {
		return t.ContainerPort, nil
	}
	return 0, echo.NewHTTPError(http.StatusBadRequest, "tile declares no port; pass one explicitly")
}
