package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// stubDocker puts a `docker` on PATH that runs script (a sh body).
func stubDocker(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func daemonAt(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := client.NewClientWithOpts(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return &Client{cli: c}
}

// The CLI has no record of the builder (its registry was lost), yet its
// buildkit container and state volume on the daemon still go.
func TestRemoveBuilderTakesDaemonObjects(t *testing.T) {
	stubDocker(t, `echo 'ERROR: no builder "x" found'; exit 1`)
	var mu sync.Mutex
	var deleted []string
	d := daemonAt(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleted = append(deleted, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := d.RemoveBuilder(context.Background(), "stackr-builder-o1"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(deleted, ",")
	if got != "buildx_buildkit_stackr-builder-o10,buildx_buildkit_stackr-builder-o10_state" {
		t.Errorf("deleted %s", got)
	}
}

// A hung `buildx stop` is killed at the timeout instead of holding buildsMu.
func TestBuildIdleStopIsBounded(t *testing.T) {
	stubDocker(t, `case "$2" in stop) sleep 20;; *) exit 1;; esac`)
	old := idleStopTimeout
	idleStopTimeout = 200 * time.Millisecond
	t.Cleanup(func() { idleStopTimeout = old })
	d := &Client{}
	start := time.Now()
	_, _ = d.Build(context.Background(), "b", t.TempDir(), "Dockerfile", "t", nil, nil, &strings.Builder{})
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("Build held up %v by a hung stop", el)
	}
	if !buildsMu.TryLock() {
		t.Fatal("buildsMu still held")
	}
	buildsMu.Unlock()
}
