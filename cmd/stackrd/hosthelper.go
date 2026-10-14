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
// ponytail: the list is fixed; a third binary in the image is one more entry.
var swapBinaries = []string{"stackrd", "stackr-install"}

// hostSwap is `stackrd worker-swap`. src is the dir holding the new
// binaries (the image root), host the host's root as mounted in the helper.
// restart restarts the worker service on the host.
type hostSwap struct {
	src, host string
	deadline  time.Duration
	restart   func(context.Context) error
	log       *slog.Logger
}

func (s hostSwap) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return slog.Default()
}

// run swaps, restarts and then waits. The controller commits by removing
// the helper, which cancels ctx (SIGTERM): nothing is restored. If the
// deadline passes first, nobody confirmed the new worker, so the old binaries
// go back and the worker restarts again; the helper then fails so the
// controller can read the outcome from its exit code.
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
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(s.deadline):
	}
	s.logger().Warn("worker-swap: not confirmed in time, putting the old binaries back")
	s.restore(bin, had)
	if err := s.restart(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("rolled back, but the restart failed: %w", err)
	}
	return errors.New("not confirmed in time; rolled back")
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

// runWorkerSwap is `stackrd worker-swap [--deadline 150s]`.
func runWorkerSwap(args []string) error {
	fs := flag.NewFlagSet("worker-swap", flag.ContinueOnError)
	deadline := fs.Duration("deadline", 150*time.Second, "how long to wait for the controller before rolling back")
	host := fs.String("host", "/host", "the host's root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return hostSwap{src: filepath.Dir(self), host: *host, deadline: *deadline, restart: chrootSystemctl(*host)}.run(ctx)
}
