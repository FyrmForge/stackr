package domain_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
)

// Saves that land while a push runs collapse into exactly one follow-up run,
// and that run sees the last save.
func TestSyncCoalesces(t *testing.T) {
	var saved atomic.Int64
	var builds atomic.Int32
	var seen []int64
	gate := make(chan struct{})
	s := &domain.Syncer{
		Build: func(context.Context) (json.RawMessage, error) {
			if builds.Add(1) == 1 {
				<-gate
			}
			seen = append(seen, saved.Load())
			return json.RawMessage(`{}`), nil
		},
		Push: func(context.Context, json.RawMessage) error { return nil },
	}
	saved.Store(1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.Sync(context.Background())
	}()
	for builds.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	const n = 5
	saved.Store(2) // a save mid-run
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Sync(context.Background())
		}()
	}
	for s.Requested() != n+1 {
		time.Sleep(time.Millisecond)
	}
	close(gate)
	wg.Wait()
	if builds.Load() != 2 || len(seen) != 2 || seen[1] != 2 {
		t.Errorf("builds = %d, seen = %v; want 2 runs, the second seeing save 2", builds.Load(), seen)
	}
}

// A cancelled request context does not cancel the run.
func TestSyncOffRequestContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &domain.Syncer{
		Build: func(c context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), c.Err() },
		Push:  func(c context.Context, _ json.RawMessage) error { return c.Err() },
	}
	if err := s.Sync(ctx); err != nil {
		t.Errorf("sync on a cancelled request = %v", err)
	}
}
