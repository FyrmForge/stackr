package store

import "context"

// TileVIPStore is the VIP of a tile's pause container on its node.
type TileVIPStore interface {
	// Get is the tile's VIP on node; "" when none is known there.
	Get(ctx context.Context, tileID, nodeID string) (string, error)
	// Set records it; vip "" forgets the tile.
	Set(ctx context.Context, tileID, nodeID, vip string) error
}

type tileVIPs struct{ q querier }

func (s tileVIPs) Get(ctx context.Context, tileID, nodeID string) (string, error) {
	var vs []string
	err := s.q.SelectContext(ctx, &vs, `SELECT vip FROM tile_vips WHERE tile_id = ? AND node_id = ?`, tileID, nodeID)
	if err != nil || len(vs) == 0 {
		return "", mapErr(err)
	}
	return vs[0], nil
}

func (s tileVIPs) Set(ctx context.Context, tileID, nodeID, vip string) error {
	if vip == "" {
		_, err := s.q.ExecContext(ctx, `DELETE FROM tile_vips WHERE tile_id = ?`, tileID)
		return mapErr(err)
	}
	_, err := s.q.ExecContext(ctx, `INSERT INTO tile_vips (tile_id, node_id, vip) VALUES (?, ?, ?)
		ON CONFLICT (tile_id) DO UPDATE SET node_id = excluded.node_id, vip = excluded.vip`, tileID, nodeID, vip)
	return mapErr(err)
}
