package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// swapDirs makes an image dir with new binaries and a fake host with old ones.
func swapDirs(t *testing.T) (src, host string) {
	t.Helper()
	src, host = t.TempDir(), t.TempDir()
	bin := filepath.Join(host, "usr/local/bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"stackrd", "stackr-install"} {
		write(t, filepath.Join(src, n), "new "+n)
		write(t, filepath.Join(bin, n), "old "+n)
	}
	return src, host
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The binaries are swapped, .prev holds the old ones, and the restart runs
// once while the helper waits for its removal.
func TestWorkerSwapKeepsNewBinaries(t *testing.T) {
	src, host := swapDirs(t)
	restarts := 0
	ctx, cancel := context.WithCancel(context.Background())
	s := hostSwap{src: src, host: host, deadline: time.Minute, restart: func(context.Context) error {
		restarts++
		cancel() // the controller removed the helper: SIGTERM
		return nil
	}}
	if err := s.run(ctx); err != nil {
		t.Fatalf("a committed swap = %v", err)
	}
	bin := filepath.Join(host, "usr/local/bin")
	for _, n := range []string{"stackrd", "stackr-install"} {
		if got := read(t, filepath.Join(bin, n)); got != "new "+n {
			t.Errorf("%s = %q, want the new one", n, got)
		}
		if got := read(t, filepath.Join(bin, n+".prev")); got != "old "+n {
			t.Errorf("%s.prev = %q, want the old one", n, got)
		}
		if _, err := os.Stat(filepath.Join(bin, n+".new")); err == nil {
			t.Errorf("%s.new left behind", n)
		}
	}
	if restarts != 1 {
		t.Fatalf("restarts = %d, want 1", restarts)
	}
}

// No commit before the deadline: the old binaries come back and the worker
// restarts a second time. The helper exits non-zero so the controller sees it.
func TestWorkerSwapRollsBackAtDeadline(t *testing.T) {
	src, host := swapDirs(t)
	restarts := 0
	s := hostSwap{src: src, host: host, deadline: 20 * time.Millisecond, restart: func(context.Context) error {
		restarts++
		return nil
	}}
	if err := s.run(context.Background()); err == nil {
		t.Fatal("a rolled-back swap must fail")
	}
	bin := filepath.Join(host, "usr/local/bin")
	for _, n := range []string{"stackrd", "stackr-install"} {
		if got := read(t, filepath.Join(bin, n)); got != "old "+n {
			t.Errorf("%s = %q, want the old one back", n, got)
		}
	}
	if restarts != 2 {
		t.Fatalf("restarts = %d, want 2", restarts)
	}
}

// A host without stackr-install (a worker from before this change) swaps
// fine, and a restart that fails restores at once.
func TestWorkerSwapOldHostAndFailedRestart(t *testing.T) {
	src, host := swapDirs(t)
	bin := filepath.Join(host, "usr/local/bin")
	if err := os.Remove(filepath.Join(bin, "stackr-install")); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s := hostSwap{src: src, host: host, deadline: time.Minute, restart: func(context.Context) error {
		calls++
		if calls == 1 {
			return os.ErrPermission
		}
		return nil
	}}
	if err := s.run(context.Background()); err == nil {
		t.Fatal("a failed restart must fail the helper")
	}
	if got := read(t, filepath.Join(bin, "stackrd")); got != "old stackrd" {
		t.Fatalf("stackrd = %q after a failed restart, want the old one", got)
	}
	if calls != 2 {
		t.Fatalf("restart calls = %d, want the failed one and a retry after the restore", calls)
	}
}

// stackrd is never absent while the swap runs: the old file is linked to
// .prev before the new one is renamed over it.
func TestWorkerSwapNeverLeavesNoBinary(t *testing.T) {
	src, host := swapDirs(t)
	bin := filepath.Join(host, "usr/local/bin", "stackrd")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := hostSwap{src: src, host: host, deadline: time.Minute, restart: func(context.Context) error {
		if _, err := os.Stat(bin); err != nil {
			t.Errorf("stackrd missing at restart: %v", err)
		}
		cancel()
		return nil
	}}
	_ = s.run(ctx)
}
