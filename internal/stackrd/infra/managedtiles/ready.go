package managedtiles

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Readiness is engine-dispatched so every managed-tile engine, current and
// future, answers the same question: is the backing store accepting requests
// (not merely "container is running")? Engines without a check pass, so a new
// engine never gets a false gate.

// Ready reports whether the instance's store is accepting requests.
func (s *Service) Ready(ctx context.Context, instance *repo.Tile) error {
	cid, err := s.ContainerID(ctx, instance)
	if err != nil || cid == "" {
		return fmt.Errorf("instance %s is not running", instance.Slug)
	}
	if probe := Engines[instance.Engine].Ready; probe != nil {
		return probe(s, ctx, instance, cid)
	}
	return nil
}

// WaitReady polls Ready until timeout.
//
// It used to check for a container once up front and give up if there was
// none, on the reasoning that waiting cannot conjure one. That was wrong in
// the case the wait exists for: one apply creates a shared instance and the
// slices cut from it, and for the first few seconds the instance has a swarm
// service but no running task yet. The pre-check fired instead of the wait, so
// every slice failed with "instance sharedpg is not running" and only a second
// apply ever worked.
//
// A container that genuinely never arrives still fails, just at the deadline
// with the same message.
func (s *Service) WaitReady(ctx context.Context, instance *repo.Tile, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := s.Ready(ctx, instance)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// slogWriter adapts the reconcile funcs' io.Writer warnings onto slog for
// callers with no deploy log to write into (instance-side self-heal).
type slogWriter struct{ instance string }

func (w slogWriter) Write(p []byte) (int, error) {
	slog.Info("provision reconcile", "instance", w.instance, "msg", strings.TrimSpace(string(p)))
	return len(p), nil
}
