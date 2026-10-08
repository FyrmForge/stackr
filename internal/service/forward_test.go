package service

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// The replica's IP is 127.0.0.1 on the env network "n", so a local
// listener stands in for the tile.
func TestDialTilePort(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	lport := ln.Addr().(*net.TCPAddr).Port
	w.fake.Details = map[string]docker.Detail{"c-api": {Running: true, Networks: map[string]string{"n": "127.0.0.1"}}}
	api := w.tile(t, "api", true)
	dead := deadPort(t) // the declared port; nothing listens there
	row, err := w.st.Tiles.Get(ctx, api.ID)
	must(t, err)
	row.ContainerPort = dead
	must(t, w.st.Tiles.Update(ctx, row))

	// explicit beats declared
	conn, got, err := w.orch.DialTile(ctx, api.ID, "", lport)
	must(t, err)
	_ = conn.Close()
	if got != lport {
		t.Errorf("explicit port = %d, want %d", got, lport)
	}
	// declared port is the default: 80 is closed here, the error names it
	if _, _, err := w.orch.DialTile(ctx, api.ID, "", 0); err == nil || !strings.Contains(err.Error(), ":"+strconv.Itoa(dead)) {
		t.Errorf("default dial = %v, want a refused dial of the declared port", err)
	}

	// no declared port and not managed: invalid input
	bare := w.tile(t, "bare", true)
	row, err = w.st.Tiles.Get(ctx, bare.ID)
	must(t, err)
	row.ContainerPort = 0
	must(t, w.st.Tiles.Update(ctx, row))
	if _, _, err := w.orch.DialTile(ctx, bare.ID, "", 0); err == nil || !strings.Contains(err.Error(), "declares no port") {
		t.Errorf("no port = %v, want the no-port error", err)
	}

	// a host-network replica has no env-network IP: the panel dials loopback
	w.fake.Details["c-api"] = docker.Detail{Running: true, HostNetwork: true}
	conn, _, err = w.orch.DialTile(ctx, api.ID, "", lport)
	must(t, err)
	_ = conn.Close()

	// a stopped tile
	down := w.tile(t, "down", false)
	if _, _, err := w.orch.DialTile(ctx, down.ID, "", lport); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("stopped = %v, want not running", err)
	}
}

// deadPort is a port that was free a moment ago.
func deadPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}
