// Package wsterm bridges a browser websocket to a docker exec TTY, the one
// implementation behind both the container terminal and the db console.
package wsterm

import (
	"context"
	"encoding/json"
	"io"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v4"
)

// resizeMsg is the client->server control message for terminal resizes; any
// other (binary) message is raw keystrokes.
type resizeMsg struct {
	Type string `json:"type"`
	Cols uint   `json:"cols"`
	Rows uint   `json:"rows"`
}

// execTTYer is the one method Serve needs off the docker runtime.
// *runtime.Runtime satisfies it; it exists so the context-lifetime test can
// stand in for docker, not to allow a second implementation.
type execTTYer interface {
	ExecTTY(ctx context.Context, containerID string, cmd []string) (io.ReadWriter, func(w, h uint) error, func(), error)
}

// Serve upgrades the request to a websocket and bridges it to cmd running on
// a TTY inside the container (nil cmd = a shell). Returns when either side
// closes.
func Serve(c echo.Context, rt execTTYer, containerID string, cmd []string) error {
	ws, err := websocket.Accept(c.Response(), c.Request(), nil)
	if err != nil {
		return nil
	}
	defer func() { _ = ws.CloseNow() }()

	// WithoutCancel: the hamr server wraps every request context in a 30s
	// ContextTimeout, and a terminal outlives that by design. Same fix as
	// wsproxy and the CLI's forward endpoint. The WithCancel on top still
	// tears the exec down when Serve returns, so nothing leaks.
	//
	// no idle timeout, a forgotten tab holds a docker exec open
	// until the browser closes the socket. wsproxy has the same property;
	// add one here if abandoned execs ever pile up.
	ctx, cancel := context.WithCancel(context.WithoutCancel(c.Request().Context()))
	defer cancel()

	stream, resize, closeExec, err := rt.ExecTTY(ctx, containerID, cmd)
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "exec failed")
		return nil
	}
	defer closeExec()

	// exec -> ws
	go func() {
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// ws -> exec (keystrokes) + resize control messages (text/JSON)
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return nil
		}
		if typ == websocket.MessageText {
			var m resizeMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" {
				_ = resize(m.Cols, m.Rows)
			}
			continue
		}
		if _, err := io.Writer(stream).Write(data); err != nil {
			return nil
		}
	}
}
