package service

import (
	"context"
	"sync"
	"time"

	ltraffic "github.com/FyrmForge/stackr/internal/service/internal/leaf/traffic"
	"github.com/FyrmForge/stackr/internal/service/internal/workerclient"
	"github.com/FyrmForge/stackr/internal/worker"
)

// Edge is one traffic lane: bytes per second From -> To. Ends are tile ids,
// slice tile ids (a consumer <-> instance lane lands on the consumer's
// slice), or the pseudo ids "proxy" and "internet".
type Edge = ltraffic.Edge

// Traffic is the env's lanes at the last 5 s sample: the first paint; the
// env's events stream carries the rest.
func (o *Orchestrator) Traffic(ctx context.Context, envID string) ([]Edge, error) {
	return o.sample.Edges(ctx, envID)
}

// TrafficSeq counts samples; a stream sends when it moves.
func (o *Orchestrator) TrafficSeq() int64 { return o.traffic.Seq() }

// workerTraffic is every connected worker's conntrack lines for its own
// tiles, read in parallel with a short timeout (the tick is 5 s); a worker
// that does not answer is left out.
func (o *Orchestrator) workerTraffic(ctx context.Context) map[string]worker.Traffic {
	if o.cluster == nil {
		return nil
	}
	o.cluster.mu.Lock()
	ext := make(map[string]*workerclient.Client, len(o.cluster.ext))
	for id, c := range o.cluster.ext {
		ext[id] = c
	}
	o.cluster.mu.Unlock()
	out := map[string]worker.Traffic{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for id, c := range ext {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if t, err := c.Traffic(cctx); err == nil {
				mu.Lock()
				out[id] = t
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}
