package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Syncer runs build-then-push one at a time. Callers that arrive while a run
// is in flight collapse into exactly one follow-up run, which rebuilds from
// the database, so a save made mid-run always reaches the proxy. Every caller
// gets the error of the run that covered it: boot reports it, a handler logs.
type Syncer struct {
	Build func(context.Context) (json.RawMessage, error) // the flow: rows + facts -> Build
	Push  func(context.Context, json.RawMessage) error   // proxy.Client.Push

	// BuildAll and PushNode, when both are set, replace Build and Push: one
	// config per ingress node, each pushed to that node's Caddy.
	BuildAll func(context.Context) (map[string]json.RawMessage, error)
	PushNode func(ctx context.Context, node string, cfg json.RawMessage) error
	// Soft names nodes whose failed push is logged and retried (Retry)
	// instead of failing the run: one ingress worker down must not fail a
	// deploy or rollback it does not serve.
	Soft func(node string) bool
	// After runs at the end of every run, under its lock: the DNS records
	// follow what was just pushed.
	After func(context.Context)

	run  sync.Mutex      // held for a whole run
	mu   sync.Mutex      // guards the counters
	want uint64          // requests so far
	done uint64          // requests covered by a finished run
	err  error           // that run's error
	soft map[string]bool // Soft nodes whose last push failed
}

// Timeout caps one run; the run never uses the caller's context, so a
// client hanging up cannot cancel a reconfigure half way.
const Timeout = 2 * time.Minute

func (s *Syncer) Sync(ctx context.Context) error {
	s.mu.Lock()
	s.want++
	mine := s.want
	s.mu.Unlock()

	s.run.Lock()
	defer s.run.Unlock()
	s.mu.Lock()
	if s.done >= mine {
		err := s.err
		s.mu.Unlock()
		return err
	}
	covers := s.want
	s.mu.Unlock()

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), Timeout)
	defer cancel()
	err := s.once(rctx)
	if s.After != nil {
		s.After(rctx)
	}
	s.mu.Lock()
	s.done, s.err = covers, err
	s.mu.Unlock()
	return err
}

// once is one build and push. A partial build (a tile left out, a node
// whose config failed) still pushes what built; its error still returns.
func (s *Syncer) once(ctx context.Context) error {
	if s.BuildAll == nil || s.PushNode == nil {
		cfg, err := s.Build(ctx)
		if cfg != nil {
			if perr := s.Push(ctx, cfg); perr != nil {
				err = perr
			}
		}
		return err
	}
	cfgs, err := s.BuildAll(ctx)
	errs := []error{err}
	for node, cfg := range cfgs { // one node down must not hold back the others
		perr := s.PushNode(ctx, node, cfg)
		soft := s.Soft != nil && s.Soft(node)
		s.mu.Lock()
		if s.soft == nil {
			s.soft = map[string]bool{}
		}
		s.soft[node] = perr != nil && soft
		s.mu.Unlock()
		switch {
		case perr != nil && soft:
			// ponytail: a deploy served by this node still succeeds; Retry
			// repairs it within a tick. Per-tile strictness if that matters.
			slog.Warn("proxy: push failed, will retry", "node", node, "err", perr)
		case perr != nil:
			errs = append(errs, fmt.Errorf("push to node %q: %w", node, perr))
		}
	}
	return errors.Join(errs...)
}

// Retry runs a sync when a Soft node's last push failed.
func (s *Syncer) Retry(ctx context.Context) error {
	s.mu.Lock()
	failed := false
	for _, f := range s.soft {
		failed = failed || f
	}
	s.mu.Unlock()
	if !failed {
		return nil
	}
	return s.Sync(ctx)
}
