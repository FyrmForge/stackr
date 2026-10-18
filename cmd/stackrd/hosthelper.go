package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/FyrmForge/stackr/internal/worker"
)

// The host helper: a privileged container from the panel image that the
// controller runs on a node (host pid, host network, the host's / at /host).
// It does the two things a node's own worker cannot do for itself: replace
// the worker's binaries and restart its service, and uninstall stackr.

const (
	hostBinDir     = "usr/local/bin"
	workerUnitName = "stackr-worker.service"
)

// swapBinaries are what the image carries next to each other and a worker
// upgrade replaces together.
var swapBinaries = worker.SwapBinaries

// hostSwap is `stackrd worker-swap`. src is the dir holding the new
// binaries (the image root), host the host's root as mounted in the helper.
// restart restarts the worker service on the host.
type hostSwap struct {
	src, host string
	deadline  time.Duration
	poll      time.Duration // how often the commit marker is looked for; 0 = 500ms
	restart   func(context.Context) error
	log       *slog.Logger
}

func (s hostSwap) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return slog.Default()
}

// run swaps, restarts and then waits. Only the controller's explicit commit
// (`worker-swap --commit`, which marks the swap state next to the binaries)
// keeps the new ones. SIGTERM (a stop or removal of the helper) or the
// deadline without a commit mean nobody confirmed the new worker, so the old
// binaries go back and the worker restarts again; the helper then fails so
// the controller can read the outcome from its exit code. If the helper
// itself dies first, the worker restores a stale swap at its next start.
func (s hostSwap) run(ctx context.Context) error {
	bin := filepath.Join(s.host, hostBinDir)
	for _, n := range swapBinaries {
		if err := copyExec(filepath.Join(s.src, n), filepath.Join(bin, n+".new")); err != nil {
			s.cleanup(bin)
			return fmt.Errorf("stage %s: %w", n, err)
		}
	}
	// Per binary: the current file is hard-linked to .prev (so it never
	// leaves its name), then the new one is renamed over it. At every
	// instant the name holds a whole, working binary.
	had := map[string]bool{}
	for _, n := range swapBinaries {
		cur, prev := filepath.Join(bin, n), filepath.Join(bin, n+".prev")
		if _, err := os.Stat(cur); err == nil {
			_ = os.Remove(prev)
			if err := os.Link(cur, prev); err != nil {
				if err := copyExec(cur, prev); err != nil {
					s.cleanup(bin)
					return fmt.Errorf("keep %s: %w", n, err)
				}
			}
			had[n] = true
		}
	}
	if err := worker.SwapPending(bin, time.Now().Add(s.deadline)); err != nil {
		s.restore(bin, had)
		return fmt.Errorf("record the swap: %w", err)
	}
	for _, n := range swapBinaries {
		if err := os.Rename(filepath.Join(bin, n+".new"), filepath.Join(bin, n)); err != nil {
			s.restore(bin, had)
			return fmt.Errorf("swap %s: %w", n, err)
		}
	}
	if err := s.restart(ctx); err != nil {
		s.restore(bin, had)
		_ = s.restart(context.WithoutCancel(ctx))
		return fmt.Errorf("restart the worker: %w", err)
	}
	s.logger().Info("worker-swap: swapped, waiting for the controller", "deadline", s.deadline)
	poll := s.poll
	if poll == 0 {
		poll = 500 * time.Millisecond
	}
	tick, deadline := time.NewTicker(poll), time.NewTimer(s.deadline)
	defer tick.Stop()
	defer deadline.Stop()
	why := "not confirmed in time"
wait:
	for {
		select {
		case <-tick.C:
			if worker.SwapCommitted(bin) {
				s.logger().Info("worker-swap: committed")
				return nil
			}
		case <-ctx.Done():
			why = "stopped before the controller committed"
			break wait
		case <-deadline.C:
			break wait
		}
	}
	if worker.SwapCommitted(bin) { // a commit that landed with the stop
		return nil
	}
	s.logger().Warn("worker-swap: " + why + ", putting the old binaries back")
	s.restore(bin, had)
	if err := s.restart(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("rolled back, but the restart failed: %w", err)
	}
	return errors.New(why + "; rolled back")
}

// restore renames each .prev back over its binary (atomic: the name always
// holds a working file). A binary the host did not have is left as is.
func (s hostSwap) restore(bin string, had map[string]bool) {
	for n := range had {
		if err := os.Rename(filepath.Join(bin, n+".prev"), filepath.Join(bin, n)); err != nil {
			s.logger().Error("worker-swap: restore failed", "binary", n, "err", err)
		}
	}
	s.cleanup(bin)
	worker.SwapClear(bin)
}

func (s hostSwap) cleanup(bin string) {
	for _, n := range swapBinaries {
		_ = os.Remove(filepath.Join(bin, n+".new"))
	}
}

// copyExec copies src to dst as an executable, synced to disk.
func copyExec(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	_ = os.Remove(dst)
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// chrootSystemctl restarts the worker unit on the host. PidHost matters:
// systemctl ignores a request that looks chrooted unless /proc/1/root is
// the host's.
func chrootSystemctl(host string) func(context.Context) error {
	return func(ctx context.Context) error {
		out, err := exec.CommandContext(ctx, "chroot", host, "systemctl", "restart", workerUnitName).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}
}

// commitSwap is `worker-swap --commit`: the controller's explicit confirmation.
func commitSwap(host string) error {
	return worker.SwapCommit(filepath.Join(host, hostBinDir))
}

// runWorkerSwap is `stackrd worker-swap [--deadline 150s]`.
func runWorkerSwap(args []string) error {
	fs := flag.NewFlagSet("worker-swap", flag.ContinueOnError)
	deadline := fs.Duration("deadline", 150*time.Second, "how long to wait for the controller before rolling back")
	host := fs.String("host", "/host", "the host's root")
	commit := fs.Bool("commit", false, "commit the swap that is waiting (the controller runs this in the helper)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *commit {
		return commitSwap(*host)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := (hostSwap{src: filepath.Dir(self), host: *host, deadline: *deadline, restart: chrootSystemctl(*host)}).run(ctx); err != nil {
		return err
	}
	// Committed: the new stackr-install sets Docker's log cap on the node. It
	// runs in a transient unit on the host, as a Docker restart kills this
	// container (and restarts the worker, which requires docker.service);
	// the delay lets the controller finish with this helper first.
	out, err := exec.Command("chroot", *host, "systemd-run", "--collect", "--on-active=15",
		"/usr/local/bin/stackr-install", "docker-logcap").CombinedOutput()
	if err != nil {
		slog.Warn("worker-swap: docker log cap not started", "err", err, "out", string(out))
	}
	return nil
}

// hostRun is `stackrd host-run uninstall --yes`: the installer from the
// image is put on the host and started in a transient systemd unit, which
// outlives this container (the uninstall removes the helper, and so this
// process). run executes one argv.
type hostRun struct {
	src, host string
	run       func(ctx context.Context, argv ...string) error
}

// stage puts the image's stackr-install on the host.
func (h hostRun) stage() error {
	dst := filepath.Join(h.host, hostBinDir, "stackr-install")
	if err := copyExec(filepath.Join(h.src, "stackr-install"), dst+".new"); err != nil {
		return fmt.Errorf("stage stackr-install: %w", err)
	}
	return os.Rename(dst+".new", dst)
}

func (h hostRun) uninstall(ctx context.Context) error {
	if err := h.stage(); err != nil {
		return err
	}
	return h.run(ctx, "chroot", h.host, "systemd-run", "--collect", "--unit", "stackr-uninstall",
		"/usr/local/bin/stackr-install", "uninstall", "--yes")
}

// logCap sets Docker's log cap on the controller after a panel upgrade, as
// worker-swap does on a worker: delayed, so the swap helper is done before a
// Docker restart.
func (h hostRun) logCap(ctx context.Context) error {
	if err := h.stage(); err != nil {
		return err
	}
	return h.run(ctx, "chroot", h.host, "systemd-run", "--collect", "--unit", "stackr-docker-logcap", "--on-active=15",
		"/usr/local/bin/stackr-install", "docker-logcap")
}

// runHostRun is `stackrd host-run uninstall --yes` or `host-run docker-logcap`.
func runHostRun(args []string) error {
	uninstall := len(args) == 2 && args[0] == "uninstall" && args[1] == "--yes"
	if !uninstall && (len(args) != 1 || args[0] != "docker-logcap") {
		return errors.New("usage: host-run uninstall --yes | host-run docker-logcap")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	h := hostRun{src: filepath.Dir(self), host: "/host", run: func(ctx context.Context, argv ...string) error {
		out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", argv[0], err, out)
		}
		return nil
	}}
	if uninstall {
		return h.uninstall(context.Background())
	}
	return h.logCap(context.Background())
}
