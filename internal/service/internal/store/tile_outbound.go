package store

import (
	"context"
	"time"
)

// TileOutbound is when a tile last reached the internet and how many bytes
// that came to.
type TileOutbound struct {
	TileID   string    `db:"tile_id" json:"tile_id"`
	LastSeen time.Time `db:"last_seen" json:"last_seen"`
	Bytes    int64     `db:"bytes" json:"bytes"`
}

// TileOutboundStore is keyed by tile.
type TileOutboundStore interface {
	// Add counts bytes more for the tile and moves its last seen to at.
	Add(ctx context.Context, tileID string, bytes int64, at time.Time) error
	// ListByEnv is every tile of the env that ever reached out.
	ListByEnv(ctx context.Context, envID string) ([]TileOutbound, error)
}

type tileOutbound struct{ q querier }

func (s tileOutbound) Add(ctx context.Context, tileID string, bytes int64, at time.Time) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO tile_outbound (tile_id, last_seen, bytes) VALUES (?, ?, ?)
		ON CONFLICT (tile_id) DO UPDATE SET last_seen = excluded.last_seen, bytes = bytes + excluded.bytes`,
		tileID, at, bytes)
	return mapErr(err)
}

func (s tileOutbound) ListByEnv(ctx context.Context, envID string) ([]TileOutbound, error) {
	var rows []TileOutbound
	err := s.q.SelectContext(ctx, &rows, `SELECT o.tile_id, o.last_seen, o.bytes FROM tile_outbound o
		JOIN tiles t ON t.id = o.tile_id WHERE t.environment_id = ? ORDER BY o.last_seen DESC`, envID)
	return rows, mapErr(err)
}
