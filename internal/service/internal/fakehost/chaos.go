package fakehost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// Knobs is chaos.json: optional, re-read when its modification time moves.
//
//	{
//	  "groups": {"containers": {"latency_ms": 200, "jitter_ms": 400, "error_rate": 0.05}},
//	  "blip_seconds": 30,
//	  "stream_line_ms": 150,
//	  "run_fail_rate": 0.1
//	}
//
// Groups: containers, networks, volumes, images, logs.
type Knobs struct {
	Groups map[string]Group `json:"groups"`
	// BlipSeconds: every call fails as if the daemon were down, for this
	// long after the file was saved. Counted from the file's mtime, so a
	// restart does not set it off again.
	BlipSeconds  int     `json:"blip_seconds"`
	StreamLineMS int     `json:"stream_line_ms"` // delay between pull, build and log lines
	RunFailRate  float64 `json:"run_fail_rate"`  // share of one-off runs that exit 1
}

type Group struct {
	LatencyMS int     `json:"latency_ms"`
	JitterMS  int     `json:"jitter_ms"`
	ErrorRate float64 `json:"error_rate"`
}

// errDown is the daemon gone, word for word.
var errDown = fmt.Errorf(
	"%w: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
	docker.ErrUnreachable)

type chaos struct {
	path string

	mu    sync.Mutex
	mtime time.Time
	k     Knobs
}

// knobs reloads the file when it changed; a bad file keeps the last good one.
func (c *chaos) knobs() (Knobs, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fi, err := os.Stat(c.path)
	if err != nil {
		c.mtime, c.k = time.Time{}, Knobs{}
		return c.k, c.mtime
	}
	if !fi.ModTime().Equal(c.mtime) {
		c.mtime = fi.ModTime()
		var k Knobs
		b, err := os.ReadFile(c.path)
		if err == nil {
			err = json.Unmarshal(b, &k)
		}
		if err != nil {
			slog.Warn("fakehost: chaos.json ignored, keeping the last settings", "err", err)
		} else {
			c.k = k
			slog.Info("fakehost: chaos loaded", "knobs", string(b))
		}
	}
	return c.k, c.mtime
}

// hit is the chaos on one call: the blip, then latency, then the error rate.
func (c *chaos) hit(ctx context.Context, group, method string) error {
	k, mtime := c.knobs()
	if k.BlipSeconds > 0 && time.Since(mtime) < time.Duration(k.BlipSeconds)*time.Second {
		return errDown
	}
	g := k.Groups[group]
	delay := time.Duration(g.LatencyMS) * time.Millisecond
	if g.JitterMS > 0 {
		delay += time.Duration(rand.IntN(g.JitterMS)) * time.Millisecond
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if g.ErrorRate > 0 && rand.Float64() < g.ErrorRate {
		return fmt.Errorf("Error response from daemon: fakehost chaos: %s failed", method) //nolint:staticcheck // the daemon's own words
	}
	return nil
}

func (c *chaos) runFails() bool {
	k, _ := c.knobs()
	return k.RunFailRate > 0 && rand.Float64() < k.RunFailRate
}

// lineDelay is the pause between streamed lines; a short default so runs
// and pulls take a visible moment.
func (c *chaos) lineDelay() time.Duration {
	if k, _ := c.knobs(); k.StreamLineMS > 0 {
		return time.Duration(k.StreamLineMS) * time.Millisecond
	}
	return 200 * time.Millisecond
}

// pause waits one line delay; false when the stream should end.
func (c *chaos) pause(ctx context.Context, quit <-chan struct{}) bool {
	select {
	case <-time.After(c.lineDelay()):
		return true
	case <-quit:
		return false
	case <-ctx.Done():
		return false
	}
}

// trickle writes lines to w, one line delay apart.
func (c *chaos) trickle(ctx context.Context, w io.Writer, lines []string) error {
	for _, l := range lines {
		if _, err := io.WriteString(w, l+"\n"); err != nil {
			return err
		}
		if !c.pause(ctx, nil) {
			return ctx.Err()
		}
	}
	return nil
}
