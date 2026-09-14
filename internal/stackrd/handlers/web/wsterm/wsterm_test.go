package wsterm_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/wsterm"
)

// fakeExec stands in for the docker runtime: the "TTY" is one end of a
// net.Pipe that echoes whatever is written to it, so a round trip through
// Serve proves the bridge is still alive.
type fakeExec struct{ closed chan struct{} }

func (f *fakeExec) ExecTTY(ctx context.Context, containerID string, cmd []string) (io.ReadWriter, func(w, h uint) error, func(), error) {
	ours, theirs := net.Pipe()
	go func() { _, _ = io.Copy(theirs, theirs) }()
	return ours, func(w, h uint) error { return nil }, func() { close(f.closed) }, nil
}

// The hamr server wraps every request context in a 30s ContextTimeout. wsterm
// used to bind its websocket and its exec stream to that context, so a
// container terminal or db console died at exactly 30.00s whether or not
// anyone was typing. Same bug wsproxy had; Serve must outlive the deadline.
func TestServeOutlivesRequestContextTimeout(t *testing.T) {
	exec := &fakeExec{closed: make(chan struct{})}

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
	e.GET("/t", func(c echo.Context) error {
		return wsterm.Serve(c, exec, "container-1", nil)
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, srv.URL+"/t", &websocket.DialOptions{HTTPClient: &http.Client{}})
	require.NoError(t, err)
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer func() { _ = conn.Close() }()

	time.Sleep(400 * time.Millisecond) // let the request deadline expire

	want := []byte("still here")
	_, err = conn.Write(want)
	require.NoError(t, err, "terminal died with the request context")
	got := make([]byte, len(want))
	_, err = io.ReadFull(conn, got)
	require.NoError(t, err, "read after request deadline")
	require.Equal(t, string(want), string(got), "round trip")

	// closing the socket must still tear the exec down, WithoutCancel drops
	// the deadline, not the cleanup
	_ = conn.Close()
	select {
	case <-exec.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("exec not closed after the websocket went away")
	}
}
