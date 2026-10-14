package worker

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// Where the Docker root is not visible (the controller's worker runs inside
// the panel container), the free space is read from the state dir instead.
func TestDiskFallsBackToTheStateDir(t *testing.T) {
	dir, err := os.MkdirTemp("", "wk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "d.sock"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ServerVersion":"29.0.0","DockerRootDir":"/nonexistent/docker-root"}`)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	s := New(Config{DockerSock: filepath.Join(dir, "d.sock"), StateDir: dir})
	var f Facts
	if err := s.dockerInfo(context.Background(), &f); err != nil {
		t.Fatalf("dockerInfo: %v", err)
	}
	if f.DiskFree == 0 || f.DiskTotal == 0 || f.DiskFree > f.DiskTotal {
		t.Errorf("disk free %d of %d", f.DiskFree, f.DiskTotal)
	}
}
