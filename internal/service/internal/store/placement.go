package store

import "context"

// IngressPickStore is the ingress placement picked at one scope (org, stack,
// env or tile). Keyed by scope, like ExitLadderStore.
type IngressPickStore interface {
	// Get is the scope's pick; "" = none (inherit).
	Get(ctx context.Context, scopeKind, scopeID string) (string, error)
	// Set picks placementID for the scope; "" clears it.
	Set(ctx context.Context, scopeKind, scopeID, placementID string) error
	// ListByPlacement is every scope that names the placement as its ingress.
	ListByPlacement(ctx context.Context, placementID string) ([]IngressPick, error)
}

type IngressPick struct {
	ScopeKind   string `db:"scope_kind" json:"scope_kind"`
	ScopeID     string `db:"scope_id" json:"scope_id"`
	PlacementID string `db:"placement_id" json:"placement_id"`
}

type ingressPicks struct{ q querier }

func (s ingressPicks) Get(ctx context.Context, scopeKind, scopeID string) (string, error) {
	var ids []string
	err := s.q.SelectContext(ctx, &ids, `SELECT placement_id FROM ingress_picks WHERE scope_kind = ? AND scope_id = ?`, scopeKind, scopeID)
	if err != nil || len(ids) == 0 {
		return "", mapErr(err)
	}
	return ids[0], nil
}

func (s ingressPicks) Set(ctx context.Context, scopeKind, scopeID, placementID string) error {
	if placementID == "" {
		_, err := s.q.ExecContext(ctx, `DELETE FROM ingress_picks WHERE scope_kind = ? AND scope_id = ?`, scopeKind, scopeID)
		return mapErr(err)
	}
	_, err := s.q.ExecContext(ctx, `INSERT INTO ingress_picks (scope_kind, scope_id, placement_id) VALUES (?, ?, ?)
		ON CONFLICT (scope_kind, scope_id) DO UPDATE SET placement_id = excluded.placement_id`, scopeKind, scopeID, placementID)
	return mapErr(err)
}

func (s ingressPicks) ListByPlacement(ctx context.Context, placementID string) ([]IngressPick, error) {
	var rows []IngressPick
	err := s.q.SelectContext(ctx, &rows, `SELECT scope_kind, scope_id, placement_id FROM ingress_picks WHERE placement_id = ? ORDER BY scope_kind, scope_id`, placementID)
	return rows, mapErr(err)
}
