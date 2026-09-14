package wsproxy_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/wsproxy"
)

// echoServer is a stand-in for the container: it upper-cases whatever it is
// sent, so the test can tell a real round-trip from an accidental loopback.
// gone receives once per connection whose handler has exited.
func echoServer(t *testing.T) (net.Listener, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gone := make(chan struct{}, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close(); gone <- struct{}{} }()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, werr := c.Write([]byte(strings.ToUpper(string(buf[:n])))); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln, gone
}

func TestServeRoundTripsBytes(t *testing.T) {
	backend, gone := echoServer(t)

	e := echo.New()
	e.GET("/f", func(c echo.Context) error {
		conn, err := net.Dial("tcp", backend.Addr().String())
		if err != nil {
			return err
		}
		// The forward handler reports the resolved container port this way;
		// asserting it below is what proves headers survive the 101.
		c.Response().Header().Set("X-Stackr-Port", "5432")
		return wsproxy.Serve(c, conn)
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, srv.URL+"/f", &websocket.DialOptions{HTTPClient: &http.Client{}})
	require.NoError(t, err)
	require.Equal(t, "5432", resp.Header.Get("X-Stackr-Port"), "port header lost across the upgrade")
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)

	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "HELLO", string(buf), "round trip")

	// Closing the client end must tear the far end down too, or every dropped
	// client leaks a connection into the container.
	_ = conn.Close()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("backend connection still open after the client closed")
	}
}

// A dial that fails must be the caller's problem, not a half-open websocket:
// Serve is only ever handed a live conn, so the handler returns an HTTP error
// and the client sees a status instead of an instant close.
func TestHandlerReportsDialFailureAsStatus(t *testing.T) {
	e := echo.New()
	e.GET("/f", func(c echo.Context) error {
		// 127.0.0.1:1 is reliably closed.
		if _, err := net.DialTimeout("tcp", "127.0.0.1:1", time.Second); err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, "cannot reach tile")
		}
		return nil
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, srv.URL+"/f", &websocket.DialOptions{HTTPClient: &http.Client{}})
	require.Error(t, err, "expected dial to fail")
	require.NotNil(t, resp, "want 502 status, got no response")
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "want 502 status, got %v", resp)
}

// The hamr server wraps every request context in a 30s ContextTimeout, which
// used to kill any tunnel older than that (an idle psql session died at
// exactly 30.00s). Serve must outlive its request context's deadline.
func TestServeOutlivesRequestContextTimeout(t *testing.T) {
	backend, _ := echoServer(t)

	e := echo.New()
	// the same middleware hamr installs, with a deadline the test can wait out
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			ctx, cancel := context.WithTimeout(c.Request().Context(), 200*time.Millisecond)
			defer cancel()
			c.SetRequest(c.Request().WithContext(ctx))
			return next(c)
		}
	})
	e.GET("/f", func(c echo.Context) error {
		conn, err := net.Dial("tcp", backend.Addr().String())
		if err != nil {
			return err
		}
		return wsproxy.Serve(c, conn)
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, srv.URL+"/f", &websocket.DialOptions{HTTPClient: &http.Client{}})
	require.NoError(t, err)
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer func() { _ = conn.Close() }()

	time.Sleep(400 * time.Millisecond) // let the request deadline expire

	_, err = conn.Write([]byte("still here"))
	require.NoError(t, err, "tunnel died with the request context")
	buf := make([]byte, 10)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err, "read after request deadline")
	require.Equal(t, "STILL HERE", string(buf), "round trip after deadline")
}
