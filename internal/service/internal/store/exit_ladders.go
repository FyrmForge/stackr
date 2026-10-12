package store

import "context"

// ExitRung is one exit node in a scope's ladder. Position 0 is tried first.
type ExitRung struct {
	ScopeKind string `db:"scope_kind" json:"scope_kind"` // org, stack, env or tile
	ScopeID   string `db:"scope_id" json:"scope_id"`
	Position  int    `db:"position" json:"position"`
	NodeID    string `db:"node_id" json:"node_id"`
}

// ExitLadderStore is keyed by scope, not by id. A ladder is replaced whole.
type ExitLadderStore interface {
	// List is one scope's rungs in position order; empty means inherit.
	List(ctx context.Context, scopeKind, scopeID string) ([]ExitRung, error)
	// ListByNode is every rung that names the node (exit-side AllowedIPs).
	ListByNode(ctx context.Context, nodeID string) ([]ExitRung, error)
	// Set replaces the scope's ladder with nodeIDs in order; none clears it.
	// Call it inside a transaction to keep the replace atomic.
	Set(ctx context.Context, scopeKind, scopeID string, nodeIDs []string) error
}

type exitLadders struct{ q querier }

// NewExitLadders binds the table to q until Tables carries it (store.go).
func NewExitLadders(q querier) ExitLadderStore { return exitLadders{q} }

func (s exitLadders) List(ctx context.Context, scopeKind, scopeID string) ([]ExitRung, error) {
	var rows []ExitRung
	err := s.q.SelectContext(ctx, &rows, `SELECT scope_kind, scope_id, position, node_id FROM exit_ladders
		WHERE scope_kind = ? AND scope_id = ? ORDER BY position`, scopeKind, scopeID)
	return rows, mapErr(err)
}

func (s exitLadders) ListByNode(ctx context.Context, nodeID string) ([]ExitRung, error) {
	var rows []ExitRung
	err := s.q.SelectContext(ctx, &rows, `SELECT scope_kind, scope_id, position, node_id FROM exit_ladders
		WHERE node_id = ? ORDER BY scope_kind, scope_id, position`, nodeID)
	return rows, mapErr(err)
}

func (s exitLadders) Set(ctx context.Context, scopeKind, scopeID string, nodeIDs []string) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM exit_ladders WHERE scope_kind = ? AND scope_id = ?`, scopeKind, scopeID); err != nil {
		return mapErr(err)
	}
	for i, n := range nodeIDs {
		if _, err := s.q.ExecContext(ctx, `INSERT INTO exit_ladders (scope_kind, scope_id, position, node_id) VALUES (?, ?, ?, ?)`,
			scopeKind, scopeID, i, n); err != nil {
			return mapErr(err)
		}
	}
	return nil
}
