package v1

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

const eventStream = "text/event-stream"

// JobEvents follows a job: an "update" event each time its log grows, an
// "end" event with the finished job. Each carries a JobLogOut whose Log is
// the new part only.
func (h *H) JobEvents() Endpoint {
	return Streamed(eventStream, func(c echo.Context) error {
		id := c.Param("job")
		return stream.Poll(c, func(ctx context.Context, offset int64) (any, int64, bool, error) {
			j, l, err := h.Orch.PollJob(ctx, id, max(offset, 0))
			return JobLogOut{j, string(l.Chunk), l.Next, l.End}, l.Next, l.End, err
		})
	})
}

// EnvEvents is the env canvas's live stream. Today one event: "traffic",
// the env's lanes ([]Edge) after each 5 s sample.
func (h *H) EnvEvents() Endpoint {
	return Streamed(eventStream, func(c echo.Context) error {
		env := envID(c)
		last, edges := int64(-1), []service.Edge{}
		return stream.PollAs(c, "traffic", func(ctx context.Context, _ int64) (any, int64, bool, error) {
			// the lanes are re-read only when a sample landed
			if seq := h.Orch.TrafficSeq(); seq != last {
				es, err := list(h.Orch.Traffic(ctx, env))
				if err != nil {
					return nil, 0, false, err
				}
				last, edges = seq, es
			}
			return edges, last, false, nil
		})
	})
}

// LogStream follows one replica's log (?container=, ?tail=), or with
// ?run= a cron or function run's log until the run ends: a "line" event
// per line.
func (h *H) LogStream() Endpoint {
	return Streamed(eventStream, func(c echo.Context) error {
		tail := 100
		if err := echo.QueryParamsBinder(c).Int("tail", &tail).BindError(); err != nil {
			return err
		}
		ctx := context.WithoutCancel(rc(c))
		follow := func() (<-chan string, func(), error) {
			return h.Orch.FollowLogs(ctx, tileID(c), c.QueryParam("container"), tail)
		}
		if run := c.QueryParam("run"); run != "" {
			follow = func() (<-chan string, func(), error) { return h.Orch.FollowRunLog(ctx, tileID(c), run, tail) }
		}
		lines, stop, err := follow()
		if err != nil {
			return err
		}
		return stream.Lines(c, lines, stop)
	}).Q("container", "run", "tail")
}

// Exec runs ?cmd= (repeated, one argv word each) in one replica
// (?container=): the request body is its stdin, the response its output
// (stdout and stderr interleaved) and the X-Exit-Code trailer its exit.
func (h *H) Exec() Endpoint {
	return Streamed("application/octet-stream", func(c echo.Context) error {
		out, wait, err := h.Orch.Terminal(
			context.WithoutCancel(rc(c)),
			tileID(c),
			c.QueryParam("container"),
			c.QueryParams()["cmd"],
			c.Request().Body,
		)
		if err != nil {
			return err
		}
		c.Response().Header().Set("Trailer", "X-Exit-Code")
		_ = stream.Pipe(c, "application/octet-stream", out)
		code := 0
		if err := wait(); err != nil {
			code = 1
			if n, ok := service.ExitCode(err); ok {
				code = n
			}
		}
		c.Response().Header().Set("X-Exit-Code", strconv.Itoa(code))
		return nil
	}).Q("cmd", "container")
}

// ForwardPortHeader carries the port the tunnel reached back on the upgrade
// response, so a client that asked for the default learns what it got.
const ForwardPortHeader = "X-Stackr-Port"

// Forward tunnels a websocket to a TCP port in one replica
// (?container=, ?port=), the server half of `stackr forward`: one socket
// per local connection, binary frames are the bytes. The dial comes first
// so every failure is an HTTP status.
func (h *H) Forward() Endpoint {
	return Streamed("websocket", func(c echo.Context) error {
		if err := stream.Check(c); err != nil {
			return err
		}
		port := 0
		if err := echo.QueryParamsBinder(c).Int("port", &port).BindError(); err != nil {
			return errs.Invalidf("port", "invalid port")
		}
		conn, port, err := h.Orch.DialTile(rc(c), tileID(c), c.QueryParam("container"), port)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		c.Response().Header().Set(ForwardPortHeader, strconv.Itoa(port))
		ws, ctx, done, err := stream.Socket(c)
		if err != nil {
			return nil // Accept already wrote the error response
		}
		defer done()
		// a failed ping ends ctx; closing conn unblocks the copy below
		go func() { <-ctx.Done(); _ = conn.Close() }()
		started := time.Now()
		slog.Info("port-forward opened", "user", who(c), "tile", tileID(c), "port", port)
		defer func() {
			slog.Info("port-forward closed", "user", who(c), "tile", tileID(c), "port", port,
				"duration", time.Since(started).Round(time.Second))
		}()
		// Half-close, not first one wins: a client that finishes sending
		// (curl, psql) sends a text {"type":"eof"} frame and must still get
		// the reply, so the tile's side is CloseWrite'n and the copy back
		// runs until it closes. A read error means the client is gone.
		ws.SetReadLimit(1 << 20)
		go func() {
			for {
				typ, b, err := ws.Read(ctx)
				if err != nil {
					_ = conn.Close()
					return
				}
				if typ == websocket.MessageBinary {
					if _, err := conn.Write(b); err != nil {
						return
					}
					continue
				}
				var m struct{ Type string }
				if json.Unmarshal(b, &m) == nil && m.Type == "eof" {
					if t, ok := conn.(*net.TCPConn); ok {
						_ = t.CloseWrite()
					}
					// keep reading: pongs are only processed inside Read
					for {
						if _, _, err := ws.Read(ctx); err != nil {
							return
						}
					}
				}
			}
		}()
		_, _ = io.Copy(websocket.NetConn(ctx, ws, websocket.MessageBinary), conn)
		_ = ws.Close(websocket.StatusNormalClosure, "") // the tile ended: a clean end, not a dropped line
		return nil
	}).Q("container", "port")
}

// Terminal is an interactive shell in one replica over a websocket
// (?container=, ?shell=). Binary frames are the terminal's bytes both ways;
// a text frame {"type":"resize","cols":n,"rows":n} in resizes it; the last
// frame out is text {"type":"exit","code":n}. Everything that can fail
// (no replica, a system container, exec create) answers an HTTP status
// before the upgrade.
func (h *H) Terminal() Endpoint {
	return Streamed("websocket", func(c echo.Context) error {
		if err := stream.Check(c); err != nil {
			return err
		}
		conn, resize, finish, err := h.Orch.Shell(context.WithoutCancel(rc(c)),
			who(c), tileID(c), c.QueryParam("container"), c.QueryParam("shell"))
		if err != nil {
			return err
		}
		ws, ctx, done, err := stream.Socket(c)
		if err != nil {
			finish()
			return nil
		}
		defer done()
		ws.SetReadLimit(1 << 20)
		out, gone := make(chan struct{}), make(chan struct{})
		go func() { // the shell's bytes to the client
			defer close(out)
			buf := make([]byte, 32<<10)
			for {
				n, err := conn.Read(buf)
				if n > 0 && ws.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
					return
				}
				if err != nil {
					return
				}
			}
		}()
		go func() { // the client's keys and resizes to the shell
			defer close(gone)
			for {
				typ, b, err := ws.Read(ctx)
				if err != nil {
					return
				}
				if typ == websocket.MessageBinary {
					if _, err := conn.Write(b); err != nil {
						return
					}
					continue
				}
				var m struct {
					Type       string
					Cols, Rows uint
				}
				if json.Unmarshal(b, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 {
					_ = resize(m.Cols, m.Rows)
				}
			}
		}()
		select {
		case <-out: // the shell ended: tell the client how
			code := finish()
			msg, _ := json.Marshal(map[string]any{"type": "exit", "code": code})
			_ = ws.Write(ctx, websocket.MessageText, msg)
			_ = ws.Close(websocket.StatusNormalClosure, "")
		case <-gone:
			finish()
		}
		return nil
	}).Q("container", "shell")
}
