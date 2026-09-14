package managedtiles

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Per-slice activity, read from the engine rather than from the wire. A
// logical database has no network identity, every consumer's packets go to
// the instance's container, so conntrack can attribute bytes to a container
// but never to one database inside it. PostgreSQL already counts per datname,
// so we ask it.

// SliceStat is one logical database's cumulative counters at a point in time.
// Xacts is transactions since the stats were last reset; Size is bytes on disk.
type SliceStat struct {
	Xacts uint64
	Size  int64
}

// SliceStats reads per-slice counters from an instance. Engines with no
// counters to read report none.
func (s *Service) SliceStats(ctx context.Context, instance *repo.Tile, dbNames []string) (map[string]SliceStat, error) {
	read := Engines[instance.Engine].SliceStats
	if read == nil || len(dbNames) == 0 {
		return nil, nil
	}
	return read(s, ctx, instance, dbNames)
}

// HasSliceStats reports whether this engine measures its slices at all.
func HasSliceStats(engine string) bool { return Engines[engine].SliceStats != nil }
