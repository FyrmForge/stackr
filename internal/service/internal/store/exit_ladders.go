package store

import "context"

// ExitRung is one exit in a scope's ladder. Position 0 is tried first.
// TargetID is a placement id for org, stack, env and tile scopes, a node id
// for the node scope, or "own".
type ExitRung struct {
	ScopeKind string `db:"scope_kind" json:"scope_kind"` // org, stack, env, tile or node
	ScopeID   string `db:"scope_id" json:"scope_id"`
	Position  int    `db:"position" json:"position"`
	TargetID  string `db:"target_id" json:"target_id"`
}

// ExitLadderStore is keyed by scope, not by id. A ladder is replaced whole.
type ExitLadderStore interface {
	// List is one scope's rungs in position order; empty means inherit.
	List(ctx context.Context, scopeKind, scopeID string) ([]ExitRung, error)
	// ListByTarget is every rung that names the target.
	ListByTarget(ctx context.Context, targetID string) ([]ExitRung, error)
	// Set replaces the scope's ladder with targetIDs in order; none clears it.
	// Call it inside a transaction to keep the replace atomic.
	Set(ctx context.Context, scopeKind, scopeID string, targetIDs []string) error
}

type exitLadders struct{ q querier }

// NewExitLadders binds the table to q until Tables carries it (store.go).
func NewExitLadders(q querier) ExitLadderStore { return exitLadders{q} }

func (s exitLadders) List(ctx context.Context, scopeKind, scopeID string) ([]ExitRung, error) {
	var rows []ExitRung
	err := s.q.SelectContext(ctx, &rows, `SELECT scope_kind, scope_id, position, target_id FROM exit_ladders
		WHERE scope_kind = ? AND scope_id = ? ORDER BY position`, scopeKind, scopeID)
	return rows, mapErr(err)
}

func (s exitLadders) ListByTarget(ctx context.Context, targetID string) ([]ExitRung, error) {
	var rows []ExitRung
	err := s.q.SelectContext(ctx, &rows, `SELECT scope_kind, scope_id, position, target_id FROM exit_ladders
		WHERE target_id = ? ORDER BY scope_kind, scope_id, position`, targetID)
	return rows, mapErr(err)
}

func (s exitLadders) Set(ctx context.Context, scopeKind, scopeID string, targetIDs []string) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM exit_ladders WHERE scope_kind = ? AND scope_id = ?`, scopeKind, scopeID); err != nil {
		return mapErr(err)
	}
	for i, n := range targetIDs {
		if _, err := s.q.ExecContext(ctx, `INSERT INTO exit_ladders (scope_kind, scope_id, position, target_id) VALUES (?, ?, ?, ?)`,
			scopeKind, scopeID, i, n); err != nil {
			return mapErr(err)
		}
	}
	return nil
}
