package domain

import (
	"context"
	"encoding/json"
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

	run  sync.Mutex // held for a whole run
	mu   sync.Mutex // guards the counters
	want uint64     // requests so far
	done uint64     // requests covered by a finished run
	err  error      // that run's error
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
	cfg, err := s.Build(rctx)
	if cfg != nil {
		// A partial build (a tile left out) still pushes; its error still returns.
		if perr := s.Push(rctx, cfg); perr != nil {
			err = perr
		}
	}
	s.mu.Lock()
	s.done, s.err = covers, err
	s.mu.Unlock()
	return err
}
