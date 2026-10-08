package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestParsePorts(t *testing.T) {
	for _, c := range []struct {
		in            string
		local, remote int
		bad           bool
	}{
		{"", 0, 0, false},
		{"8080", 8080, 0, false},
		{"5433:5432", 5433, 5432, false},
		{"0", 0, 0, true},
		{"70000", 0, 0, true},
		{"a:5432", 0, 0, true},
		{"5433:x", 0, 0, true},
		{"5433:", 0, 0, true},
	} {
		l, r, err := parsePorts(c.in)
		if (err != nil) != c.bad || (!c.bad && (l != c.local || r != c.remote)) {
			t.Errorf("parsePorts(%q) = %d %d %v", c.in, l, r, err)
		}
	}
}

func TestForwardRefusesJSON(t *testing.T) {
	useServer(t, "http://127.0.0.1:1")
	if code, _, errw := cli(t, "forward", "api", "--stack", "s", "--env", "e", "--json"); code != 2 ||
		!strings.Contains(errw, "no machine-readable output") {
		t.Errorf("forward --json = %d %q", code, errw)
	}
}

// One local connection tunnels through the server: the fake echoes with a
// prefix, the client half-closes after sending and still gets the reply.
func TestForwardTunnels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Stackr-Port", "5432")
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
		b := make([]byte, 4)
		if _, err := io.ReadFull(nc, b); err == nil {
			time.Sleep(50 * time.Millisecond)
			_, _ = nc.Write(append([]byte("re:"), b...))
		}
		_ = nc.Close()
	}))
	t.Cleanup(srv.Close)
	useServer(t, srv.URL)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	local := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var out, errw strings.Builder
	done := make(chan int)
	go func() {
		done <- run(ctx, []string{"forward", "api", "--stack", "shop", "--env", "dev", "--port", strconv.Itoa(local)},
			strings.NewReader(""), &out, &errw, false)
	}()
	var c net.Conn
	for range 100 {
		if c, err = net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(local)); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("ping"))
	_ = c.(*net.TCPConn).CloseWrite()
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "re:ping" {
		t.Errorf("reply = %q, %v", b, err)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit = %d %s", code, errw.String())
	}
	if want := "Forwarding 127.0.0.1:" + strconv.Itoa(local) + " -> api:5432 (Ctrl-C to stop)"; !strings.Contains(out.String(), want) {
		t.Errorf("out = %q, want %q", out.String(), want)
	}
}
