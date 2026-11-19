package store

import (
	"context"
	"log/slog"
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
	// AddAll counts each row's bytes more for its tile and moves its last
	// seen to the row's, all in one transaction. A tile that no longer exists
	// is skipped; a row that fails is logged and the rest go on.
	AddAll(ctx context.Context, rows []TileOutbound) error
	// ListByEnv is every tile of the env that ever reached out.
	ListByEnv(ctx context.Context, envID string) ([]TileOutbound, error)
}

type tileOutbound struct{ q querier }

func (s tileOutbound) AddAll(ctx context.Context, rows []TileOutbound) error {
	return inTx(ctx, s.q, func(q querier) error {
		for _, r := range rows {
			// WHERE EXISTS: a deleted tile has no row to hang this on (the
			// foreign key would refuse it, every flush).
			if _, err := q.ExecContext(ctx, `INSERT INTO tile_outbound (tile_id, last_seen, bytes)
				SELECT ?, ?, ? WHERE EXISTS (SELECT 1 FROM tiles WHERE id = ?)
				ON CONFLICT (tile_id) DO UPDATE SET last_seen = excluded.last_seen, bytes = bytes + excluded.bytes`,
				r.TileID, r.LastSeen, r.Bytes, r.TileID); err != nil {
				slog.Warn("tile outbound", "tile", r.TileID, "err", err)
			}
		}
		return nil
	})
}

func (s tileOutbound) ListByEnv(ctx context.Context, envID string) ([]TileOutbound, error) {
	var rows []TileOutbound
	err := s.q.SelectContext(ctx, &rows, `SELECT o.tile_id, o.last_seen, o.bytes FROM tile_outbound o
		JOIN tiles t ON t.id = o.tile_id WHERE t.environment_id = ? ORDER BY o.last_seen DESC`, envID)
	return rows, mapErr(err)
}
