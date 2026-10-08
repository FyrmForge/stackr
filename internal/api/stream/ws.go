package stream

import (
	"context"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v4"
)

// PingEvery probes an open websocket; a laptop that went away without a
// FIN (lid shut, VPN dropped) is noticed within two of these.
var PingEvery = 30 * time.Second

// Socket upgrades the request after the caller has done everything that can
// still fail as an HTTP status (resolve, exec create, dial): past the
// upgrade only a close code reaches the client. ctx outlives the server's
// per-request timeout and ends when either side goes or a ping fails; done
// must be deferred. A failed upgrade has already written its response, so
// the caller returns nil.
// Origin: coder/websocket's default refuses a cross-origin upgrade, which
// keeps another site from opening a shell with the user's cookie.
func Socket(c echo.Context) (ws *websocket.Conn, ctx context.Context, done func(), err error) {
	ws, err = websocket.Accept(c.Response(), c.Request(), nil)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(c.Request().Context()))
	go keepalive(ctx, cancel, ws)
	return ws, ctx, func() { cancel(); _ = ws.CloseNow() }, nil
}

func keepalive(ctx context.Context, cancel func(), ws *websocket.Conn) {
	t := time.NewTicker(PingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, stop := context.WithTimeout(ctx, PingEvery)
			err := ws.Ping(pctx)
			stop()
			if err != nil {
				cancel()
				_ = ws.CloseNow()
				return
			}
		}
	}
}
