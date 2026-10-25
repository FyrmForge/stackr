package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

type fakeCaddy struct {
	path, body string
	status     int
}

func (f *fakeCaddy) Caddy(_ context.Context, _, path string, body []byte) (*http.Response, error) {
	f.path, f.body = path, string(body)
	return &http.Response{StatusCode: f.status, Status: "x", Body: io.NopCloser(strings.NewReader("no"))}, nil
}

// The controller's config goes to the local push, a worker's through its
// Caddy pass-through, and a node with no connection is an error.
func TestProxyPushPerNode(t *testing.T) {
	var local string
	w := &fakeCaddy{status: 200}
	push := proxyPush(
		func(context.Context) (string, error) { return "ctl", nil },
		func(_ context.Context, c json.RawMessage) error { local = string(c); return nil },
		func(id string) caddyNode {
			if id == "w1" {
				return w
			}
			return nil
		},
		func(_ context.Context, id string) error { return errs.ErrBusy })
	ctx := context.Background()
	if err := push(ctx, "ctl", json.RawMessage(`{"a":1}`)); err != nil || local != `{"a":1}` {
		t.Fatalf("controller: %v %q", err, local)
	}
	if err := push(ctx, "w1", json.RawMessage(`{"b":2}`)); err != nil || w.path != "/load" || w.body != `{"b":2}` {
		t.Fatalf("worker: %v %+v", err, w)
	}
	if err := push(ctx, "w2", nil); err == nil {
		t.Fatal("unconnected node pushed")
	}
	w.status = 400
	if err := push(ctx, "w1", nil); err == nil {
		t.Fatal("rejected push reported ok")
	}
}

// Only a controller tile served by the controller's own Caddy goes by
// container name; a worker ingress in front of it needs the VIP.
func TestNeedsVIP(t *testing.T) {
	for _, c := range []struct {
		node, ingress string
		want          bool
	}{
		{"ctl", "ctl", false},
		{"ctl", "h4", true},
		{"w1", "ctl", true},
		{"w1", "w1", true},
	} {
		if got := needsVIP(c.node, "ctl", c.ingress); got != c.want {
			t.Errorf("needsVIP(%s, ingress %s) = %v, want %v", c.node, c.ingress, got, c.want)
		}
	}
}

// A swap copy still beside the database at boot is reported and kept.
func TestControllerLogCapWarnsOnSwapCopy(t *testing.T) {
	n := newTwoNodes(t)
	cp := n.orch.cfg.DBPath + ".pre-9.9.9"
	if err := os.WriteFile(cp, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	n.orch.controllerLogCap(context.Background())
	if !strings.Contains(buf.String(), cp) {
		t.Errorf("no warning naming %s: %s", cp, buf.String())
	}
	if _, err := os.Stat(cp); err != nil {
		t.Errorf("copy removed at boot: %v", err)
	}
}
