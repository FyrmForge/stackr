package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/FyrmForge/stackr/internal/service"
)

const termPath = "/orgs/acme/stacks/shop/envs/dev/tiles/api/terminal"

// The terminal answers its failures as HTTP before the upgrade: 401 with no
// key, 409 with no replica; with one it carries bytes both ways, resizes
// reach the exec and the shell's exit code is the last frame.
func TestTerminal(t *testing.T) {
	w := newWorld(t)
	if code, _ := w.do(t, "", "GET", termPath, ""); code != 401 {
		t.Errorf("no key = %d, want 401", code)
	}
	srv := httptest.NewServer(w.h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := http.Header{"Authorization": {"Bearer " + w.owner}}
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1" + termPath
	if _, resp, _ := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h}); resp == nil || resp.StatusCode != 409 {
		t.Errorf("no replica = %v, want 409", resp)
	}

	fake := w.env.Docker
	fake.Containers = []service.Container{{
		ID: "c-1", State: "running",
		Labels: map[string]string{"stackr.tile": w.tile.ID, "stackr.role": "replica"},
	}}
	fake.TTYExit = 3
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	shell := fake.TTYServer

	if err := ws.Write(ctx, websocket.MessageBinary, []byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 3)
	if _, err := io.ReadFull(shell, got); err != nil || string(got) != "ls\r" {
		t.Fatalf("shell got %q, %v", got, err)
	}
	go func() { _, _ = shell.Write([]byte("out")) }()
	if typ, b, err := ws.Read(ctx); err != nil || typ != websocket.MessageBinary || string(b) != "out" {
		t.Fatalf("client got %v %q, %v", typ, b, err)
	}

	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if len(fake.ResizeLog()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r := fake.ResizeLog(); len(r) != 1 || r[0] != [2]uint{120, 40} {
		t.Errorf("resizes = %v, want [120 40]", r)
	}

	_ = shell.Close() // the shell exits
	typ, b, err := ws.Read(ctx)
	var m struct {
		Type string
		Code int
	}
	if err != nil || typ != websocket.MessageText || json.Unmarshal(b, &m) != nil || m.Type != "exit" || m.Code != 3 {
		t.Errorf("last frame = %v %q, %v; want exit 3", typ, b, err)
	}
}

// A plain GET (a cross-site link) or a cross-origin upgrade is refused before
// the exec is created: nothing runs in the tile.
func TestTerminalRefusesBeforeExec(t *testing.T) {
	w := newWorld(t)
	w.env.Docker.Containers = []service.Container{{
		ID: "c-1", State: "running",
		Labels: map[string]string{"stackr.tile": w.tile.ID, "stackr.role": "replica"},
	}}
	if code, _ := w.do(t, w.owner, "GET", termPath+"?shell=reboot", ""); code != 426 {
		t.Errorf("plain GET = %d, want 426", code)
	}
	srv := httptest.NewServer(w.h)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1"+termPath+"?shell=reboot", nil)
	req.Header.Set("Authorization", "Bearer "+w.owner)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("cross-origin = %d, want 403", resp.StatusCode)
	}
	for _, c := range w.env.Docker.Calls() {
		if strings.HasPrefix(c.String(), "ExecTTY") {
			t.Errorf("exec ran: %s", c)
		}
	}
}
