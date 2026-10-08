package api_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

const forwardPath = "/api/v1/orgs/acme/stacks/shop/envs/dev/tiles/api/forward"

// A local listener stands in for the tile: it reads the request, answers
// with more than one websocket read limit and closes; the reply must come
// through in full and end with the tile's close.
func TestForwardTunnel(t *testing.T) {
	w := newWorld(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 4)
				if _, err := io.ReadFull(c, buf); err != nil {
					return
				}
				// a reply bigger than one websocket read limit
				_, _ = c.Write(append(buf, make([]byte, 100<<10)...))
			}()
		}
	}()
	w.env.Replica(w.tile.ID, "127.0.0.1")
	dl, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := dl.Addr().(*net.TCPAddr).Port
	_ = dl.Close()
	row, err := w.env.Store.Tiles.Get(context.Background(), w.tile.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.ContainerPort = dead // nothing listens there
	if err := w.env.Store.Tiles.Update(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(w.h)
	t.Cleanup(srv.Close)
	port := ln.Addr().(*net.TCPAddr).Port

	dial := func(key, q string) (*websocket.Conn, *http.Response, error) {
		h := http.Header{}
		if key != "" {
			h.Set("Authorization", "Bearer "+key)
		}
		return websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+forwardPath+q,
			&websocket.DialOptions{HTTPHeader: h})
	}

	if _, res, err := dial("", ""); err == nil || res == nil || res.StatusCode != 401 {
		t.Errorf("no key = %v %v, want 401", err, res)
	}
	ws, res, err := dial(w.owner, "?port="+itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Header.Get("X-Stackr-Port"); got != itoa(port) {
		t.Errorf("X-Stackr-Port = %q, want %d", got, port)
	}
	nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	if _, err := nc.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(nc)
	if err != nil || len(b) != 4+100<<10 || string(b[:4]) != "ping" {
		t.Fatalf("reply = %d bytes, %v; want the full echo", len(b), err)
	}

	// the declared port listens there: a status, not a close code
	if _, res, err := dial(w.owner, ""); err == nil || res == nil || res.StatusCode != 409 {
		t.Errorf("refused dial = %v %v, want 409 before the upgrade", err, res)
	}
}

// A client that finishes sending signals eof: the tile sees EOF on its read
// and its reply still arrives.
func TestForwardHalfClose(t *testing.T) {
	w := newWorld(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		b, _ := io.ReadAll(c) // returns only once the client's eof closed the write side
		_, _ = c.Write(append([]byte("got:"), b...))
	}()
	w.env.Replica(w.tile.ID, "127.0.0.1")
	srv := httptest.NewServer(w.h)
	t.Cleanup(srv.Close)
	ctx := context.Background()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+forwardPath+"?port="+itoa(ln.Addr().(*net.TCPAddr).Port),
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + w.owner}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageBinary, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"eof"}`)); err != nil {
		t.Fatal(err)
	}
	nc := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	if b, err := io.ReadAll(nc); err != nil || string(b) != "got:ping" {
		t.Errorf("reply = %q, %v; want got:ping", b, err)
	}
}

// A tile with no declared port and no ?port= is a 400 before the upgrade.
func TestForwardNoPort(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	row, err := w.env.Store.Tiles.Get(ctx, w.tile.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.ContainerPort = 0
	if err := w.env.Store.Tiles.Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	w.env.Replica(w.tile.ID, "127.0.0.1")
	code, body := w.do(t, w.owner, "GET", "/orgs/acme/stacks/shop/envs/dev/tiles/api/forward", "")
	if code != 400 || !strings.Contains(body, "declares no port") {
		t.Errorf("no port = %d %s, want 400", code, body)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
