package store

import "context"

// IngressPickStore is the ingress node picked at one scope (org, stack, env
// or tile). Keyed by scope, like ExitLadderStore.
type IngressPickStore interface {
	// Get is the scope's pick; "" = none (inherit).
	Get(ctx context.Context, scopeKind, scopeID string) (string, error)
	// Set picks nodeID for the scope; "" clears it.
	Set(ctx context.Context, scopeKind, scopeID, nodeID string) error
	// ListByNode is every scope that names the node as its ingress.
	ListByNode(ctx context.Context, nodeID string) ([]IngressPick, error)
}

type IngressPick struct {
	ScopeKind string `db:"scope_kind" json:"scope_kind"`
	ScopeID   string `db:"scope_id" json:"scope_id"`
	NodeID    string `db:"node_id" json:"node_id"`
}

type ingressPicks struct{ q querier }

func (s ingressPicks) Get(ctx context.Context, scopeKind, scopeID string) (string, error) {
	var ids []string
	err := s.q.SelectContext(ctx, &ids, `SELECT node_id FROM ingress_picks WHERE scope_kind = ? AND scope_id = ?`, scopeKind, scopeID)
	if err != nil || len(ids) == 0 {
		return "", mapErr(err)
	}
	return ids[0], nil
}

func (s ingressPicks) Set(ctx context.Context, scopeKind, scopeID, nodeID string) error {
	if nodeID == "" {
		_, err := s.q.ExecContext(ctx, `DELETE FROM ingress_picks WHERE scope_kind = ? AND scope_id = ?`, scopeKind, scopeID)
		return mapErr(err)
	}
	_, err := s.q.ExecContext(ctx, `INSERT INTO ingress_picks (scope_kind, scope_id, node_id) VALUES (?, ?, ?)
		ON CONFLICT (scope_kind, scope_id) DO UPDATE SET node_id = excluded.node_id`, scopeKind, scopeID, nodeID)
	return mapErr(err)
}

func (s ingressPicks) ListByNode(ctx context.Context, nodeID string) ([]IngressPick, error) {
	var rows []IngressPick
	err := s.q.SelectContext(ctx, &rows, `SELECT scope_kind, scope_id, node_id FROM ingress_picks WHERE node_id = ? ORDER BY scope_kind, scope_id`, nodeID)
	return rows, mapErr(err)
}
