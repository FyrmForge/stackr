// Package outbound keeps which tiles reached the internet and how many bytes
// that came to. The sampling tick adds in memory; Flush writes the sums once
// in a while, so a 5 s tick is not a write per tile.
package outbound

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// FlushEvery is how often the sums reach the table.
const FlushEvery = time.Minute

type Leaf struct {
	rows store.TileOutboundStore

	mu      sync.Mutex
	pending map[string]store.TileOutbound
	flushed time.Time
}

func New(rows store.TileOutboundStore) *Leaf {
	return &Leaf{rows: rows, pending: map[string]store.TileOutbound{}}
}

// Add counts bytes more for the tile, seen at at (memory only).
func (l *Leaf) Add(tileID string, bytes int64, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.pending[tileID]
	p.TileID, p.Bytes, p.LastSeen = tileID, p.Bytes+bytes, at
	l.pending[tileID] = p
}

// FlushDue writes the sums when FlushEvery has passed since the last write.
// A failed write, or a row of it that failed, keeps its sum for the next try.
func (l *Leaf) FlushDue(ctx context.Context, now time.Time) error {
	l.mu.Lock()
	if now.Sub(l.flushed) < FlushEvery || len(l.pending) == 0 {
		l.mu.Unlock()
		return nil
	}
	rows := make([]store.TileOutbound, 0, len(l.pending))
	for _, r := range l.pending {
		rows = append(rows, r)
	}
	l.mu.Unlock()
	failed, err := l.rows.AddAll(ctx, rows)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range rows {
		if slices.Contains(failed, r.TileID) {
			continue // not persisted: its sum stays for the next flush
		}
		// bytes added while writing stay pending
		if p := l.pending[r.TileID]; p.Bytes == r.Bytes {
			delete(l.pending, r.TileID)
		} else {
			p.Bytes -= r.Bytes
			l.pending[r.TileID] = p
		}
	}
	l.flushed = now
	return nil
}

// ListByEnv is every tile of the env that ever reached out, newest first.
func (l *Leaf) ListByEnv(ctx context.Context, envID string) ([]store.TileOutbound, error) {
	return l.rows.ListByEnv(ctx, envID)
}
