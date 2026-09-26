package service

import (
	"context"

	ltraffic "github.com/FyrmForge/stackr/internal/service/internal/leaf/traffic"
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
