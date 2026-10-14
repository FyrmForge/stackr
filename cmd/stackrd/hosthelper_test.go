package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/worker"
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
	s := hostSwap{src: src, host: host, deadline: time.Minute, poll: time.Millisecond, restart: func(context.Context) error {
		restarts++
		go func() { // the controller's explicit commit
			time.Sleep(5 * time.Millisecond)
			_ = worker.SwapCommit(filepath.Join(host, hostBinDir))
		}()
		return nil
	}}
	if err := s.run(context.Background()); err != nil {
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

// M1: SIGTERM (a stop, a removeHelpers, a node remove, a cancelled rollout)
// is not a commit: the old binaries come back and the worker restarts.
func TestWorkerSwapSigtermRollsBack(t *testing.T) {
	src, host := swapDirs(t)
	restarts := 0
	ctx, cancel := context.WithCancel(context.Background())
	s := hostSwap{src: src, host: host, deadline: time.Minute, poll: time.Millisecond, restart: func(context.Context) error {
		restarts++
		cancel()
		return nil
	}}
	if err := s.run(ctx); err == nil {
		t.Fatal("a stopped, uncommitted swap must fail")
	}
	bin := filepath.Join(host, hostBinDir)
	for _, n := range swapBinaries {
		if got := read(t, filepath.Join(bin, n)); got != "old "+n {
			t.Errorf("%s = %q, want the old one back", n, got)
		}
	}
	if restarts != 2 {
		t.Fatalf("restarts = %d, want 2", restarts)
	}
	if ok, _ := worker.RestoreStaleSwap(bin, time.Now().Add(time.Hour)); ok {
		t.Fatal("swap state left behind after the rollback")
	}
}

// `worker-swap --commit` is how the controller commits.
func TestWorkerSwapCommitCommand(t *testing.T) {
	_, host := swapDirs(t)
	bin := filepath.Join(host, hostBinDir)
	if err := commitSwap(host); err == nil {
		t.Fatal("commit with nothing pending must fail")
	}
	_ = worker.SwapPending(bin, time.Now().Add(time.Minute))
	if err := commitSwap(host); err != nil || !worker.SwapCommitted(bin) {
		t.Fatalf("commit = %v, committed = %v", err, worker.SwapCommitted(bin))
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
	s := hostSwap{src: src, host: host, deadline: 10 * time.Millisecond, restart: func(context.Context) error {
		if _, err := os.Stat(bin); err != nil {
			t.Errorf("stackrd missing at restart: %v", err)
		}
		return nil
	}}
	_ = s.run(context.Background())
}

// host-run uninstall puts the image's installer on the host and starts it in
// a transient unit that outlives the helper container.
func TestHostRunUninstall(t *testing.T) {
	src, host := swapDirs(t)
	var got []string
	h := hostRun{src: src, host: host, run: func(_ context.Context, argv ...string) error {
		got = argv
		return nil
	}}
	if err := h.uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b := read(t, filepath.Join(host, "usr/local/bin/stackr-install")); b != "new stackr-install" {
		t.Fatalf("host installer = %q, want the image's", b)
	}
	want := []string{"chroot", host, "systemd-run", "--collect", "--unit", "stackr-uninstall",
		"/usr/local/bin/stackr-install", "uninstall", "--yes"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("command = %v, want %v", got, want)
	}
	if err := runHostRun([]string{"uninstall"}); err == nil {
		t.Fatal("host-run uninstall without --yes was accepted")
	}
}
