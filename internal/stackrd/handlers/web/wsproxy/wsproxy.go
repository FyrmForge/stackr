// Package wsproxy bridges a websocket to a plain TCP connection, the server
// half of `stackr forward`. Same shape as wsterm, but with a socket on the far
// side instead of a docker exec TTY, and no control messages: every frame is
// opaque protocol traffic (postgres wire, HTTP, whatever the tile speaks).
package wsproxy

import (
	"context"
	"io"
	"net"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v4"
)

// Serve upgrades the request and pipes it to conn until either side closes.
// conn is closed on return.
//
// Dial before calling: once the response is upgraded there is no way to report
// a failure as an HTTP status, so "container not running" and "connection
// refused" must both be decided by the caller first.
func Serve(c echo.Context, conn net.Conn) error {
	defer func() { _ = conn.Close() }()

	ws, err := websocket.Accept(c.Response(), c.Request(), nil)
	if err != nil {
		return nil // Accept already wrote the error response
	}
	defer func() { _ = ws.CloseNow() }()

	// NetConn lifts the read limit (default 32KiB/message), which matters here:
	// frames carry bulk data, not the small control messages wsterm sends.
	//
	// WithoutCancel: the hamr server wraps every request context in a 30s
	// ContextTimeout, which would kill any tunnel older than that, an idle
	// psql session died at exactly 30.00s. A tunnel's lifetime is decided by
	// its two ends: either side closing makes a copy return, which closes both
	// conns via the defers. The request context has nothing to add.
	wc := websocket.NetConn(context.WithoutCancel(c.Request().Context()), ws, websocket.MessageBinary)

	// Half-close, not first-one-wins: a client that finishes sending (curl,
	// `nc -N`, a psql query) ends the client→tile copy immediately, and
	// returning there would close the tunnel before the reply came back. Tell
	// the tile the request is over, then keep pumping until the tile closes,
	// which is what the relay's own pipe() already does one hop further in.
	go func() {
		_, _ = io.Copy(conn, wc)
		if t, ok := conn.(*net.TCPConn); ok {
			_ = t.CloseWrite()
		}
	}()
	_, _ = io.Copy(wc, conn)
	return nil
}
