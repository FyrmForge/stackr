package store

import "context"

// Position is where one card sits on one canvas.
type Position struct {
	ScopeKind string `db:"scope_kind" json:"scope_kind"`
	ScopeID   string `db:"scope_id" json:"scope_id"`
	NodeID    string `db:"node_id" json:"node_id"`
	X         int    `db:"x" json:"x"`
	Y         int    `db:"y" json:"y"`
}

// PositionStore is keyed by (scope, node), not by id.
type PositionStore interface {
	ListByScope(ctx context.Context, scopeKind, scopeID string) ([]Position, error)
	// Put inserts or moves one card.
	Put(ctx context.Context, p Position) error
	DeleteScope(ctx context.Context, scopeKind, scopeID string) error
}

type positions struct{ q querier }

func (s positions) ListByScope(ctx context.Context, scopeKind, scopeID string) ([]Position, error) {
	var rows []Position
	err := s.q.SelectContext(
		ctx,
		&rows,
		`SELECT scope_kind, scope_id, node_id, x, y FROM positions WHERE scope_kind = ? AND scope_id = ?`,
		scopeKind,
		scopeID,
	)
	return rows, mapErr(err)
}

func (s positions) Put(ctx context.Context, p Position) error {
	_, err := s.q.NamedExecContext(ctx, `INSERT INTO positions (scope_kind, scope_id, node_id, x, y)
		VALUES (:scope_kind, :scope_id, :node_id, :x, :y)
		ON CONFLICT (scope_kind, scope_id, node_id) DO UPDATE SET x = excluded.x, y = excluded.y`, p)
	return mapErr(err)
}

func (s positions) DeleteScope(ctx context.Context, scopeKind, scopeID string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM positions WHERE scope_kind = ? AND scope_id = ?`, scopeKind, scopeID)
	return mapErr(err)
}
