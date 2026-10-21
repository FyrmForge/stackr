// Command stackr-observe runs VictoriaMetrics, and VictoriaLogs when asked,
// in the stackr-observe container: it starts each, restarts one that exits
// with a backoff, and passes SIGTERM/SIGINT on so both flush before the
// container stops. Each serves a unix socket in the directory the panel
// mounts; the container has no network.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	var (
		run       = flag.String("run", "/run/stackr-observe", "directory for the sockets")
		data      = flag.String("data", "/data", "storage root")
		retention = flag.String("retention", "30d", "metrics retention")
		minFree   = flag.String("min-free", "5GB", "free disk below which metrics stop being written")
		logs      = flag.Bool("logs", false, "also run VictoriaLogs")
		logsKeep  = flag.String("logs-retention", "7d", "logs retention")
		logsMax   = flag.String("logs-max-size", "10GB", "logs disk use above which the oldest days are dropped")
	)
	flag.Parse()
	children := [][]string{{
		"/victoria-metrics-prod",
		"-httpListenAddr=unix:" + *run + "/metrics.sock",
		"-storageDataPath=" + *data + "/metrics",
		"-retentionPeriod=" + *retention,
		"-storage.minFreeDiskSpaceBytes=" + *minFree,
	}}
	if *logs {
		children = append(children, []string{
			"/victoria-logs-prod",
			"-httpListenAddr=unix:" + *run + "/logs.sock",
			"-storageDataPath=" + *data + "/logs",
			"-retentionPeriod=" + *logsKeep,
			"-retention.maxDiskSpaceUsageBytes=" + *logsMax,
			"-delete.enable", // the logs socket is the panel's alone; it purges an env's logs
		})
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	var wg sync.WaitGroup
	for _, argv := range children {
		wg.Go(func() { keep(ctx, argv) })
	}
	wg.Wait()
}

// keep runs argv until ctx ends, restarting it after an exit: 1 s, doubling
// to 30 s, back to 1 s after a minute up. On ctx's end the child gets
// SIGTERM and the time it needs to flush.
func keep(ctx context.Context, argv []string) {
	wait := time.Second
	for {
		// A socket left by a crash refuses the next listen.
		for _, a := range argv[1:] {
			if p, ok := strings.CutPrefix(a, "-httpListenAddr=unix:"); ok {
				_ = os.Remove(p)
			}
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		start := time.Now()
		if err := cmd.Start(); err != nil {
			log.Printf("%s: %v", argv[0], err)
		} else {
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				log.Printf("%s exited: %v", argv[0], err)
			case <-ctx.Done():
				_ = cmd.Process.Signal(syscall.SIGTERM)
				log.Printf("%s stopped: %v", argv[0], <-done)
				return
			}
		}
		if time.Since(start) > time.Minute {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, 30*time.Second)
	}
}
