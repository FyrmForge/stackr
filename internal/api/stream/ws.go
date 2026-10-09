package stream

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v4"
)

// PingEvery probes an open websocket; a laptop that went away without a
// FIN (lid shut, VPN dropped) is noticed within two of these.
var PingEvery = 30 * time.Second

// Check refuses what Socket would, before the caller runs anything: a
// handshake coder/websocket would reject (same checks and statuses as its
// verifyClientRequest) or a cross-origin one (an Origin whose host is not the
// Host, its own rule; Origin is judged before the version and key). Both are plain statuses, so a cross-site GET with the
// session cookie cannot start an exec or a dial.
func Check(c echo.Context) error {
	r := c.Request()
	if !r.ProtoAtLeast(1, 1) || !hasToken(r.Header, "Connection", "upgrade") || !hasToken(r.Header, "Upgrade", "websocket") {
		return echo.NewHTTPError(http.StatusUpgradeRequired, "websocket upgrade required")
	}
	if r.Method != http.MethodGet {
		return echo.NewHTTPError(http.StatusMethodNotAllowed, "websocket handshake must be GET")
	}
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || !strings.EqualFold(u.Host, r.Host) {
			return echo.NewHTTPError(http.StatusForbidden, "cross-origin websocket refused")
		}
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return echo.NewHTTPError(http.StatusBadRequest, "unsupported websocket version")
	}
	keys := r.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "bad Sec-WebSocket-Key")
	}
	if v, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keys[0])); err != nil || len(v) != 16 {
		return echo.NewHTTPError(http.StatusBadRequest, "bad Sec-WebSocket-Key")
	}
	return nil
}

// hasToken reports whether any comma-separated token of the key header equals
// token, ignoring case.
func hasToken(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

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
