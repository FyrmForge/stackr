package service

import (
	"sync"
	"sync/atomic"
	"testing"
)

// A Kick racing the shutdown starts nothing once Close has run, and Close
// never races a late Kick.
func TestCoalesceCloseStopsLateKicks(t *testing.T) {
	var runs atomic.Int32
	k := &coalesce{}
	k.fn = func() { runs.Add(1) }
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(k.Kick)
	}
	k.Close()
	wg.Wait()
	n := runs.Load()
	k.Kick()
	k.Wait()
	if runs.Load() != n {
		t.Fatalf("a Kick after Close ran: %d then %d", n, runs.Load())
	}
}
