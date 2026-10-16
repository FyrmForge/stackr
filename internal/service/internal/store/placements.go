package store

import (
	"context"
	"time"
)

// Placement is a row of placements: a named place envs belong to. NodeID is
// the node it stands on, "" = no node yet; it may name a forgotten node.
type Placement struct {
	ID        string    `db:"id" json:"id"`
	Name      string    `db:"name" json:"name"`
	NodeID    string    `db:"node_id" json:"node_id"`
	OpenToAll bool      `db:"open_to_all" json:"open_to_all"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type PlacementStore interface {
	Create(ctx context.Context, p Placement) error
	Get(ctx context.Context, id string) (Placement, error)
	GetByName(ctx context.Context, name string) (Placement, error)
	List(ctx context.Context) ([]Placement, error)
	// ListByNode is every placement standing on the node.
	ListByNode(ctx context.Context, nodeID string) ([]Placement, error)
	// SetNode is the one writer of the node; "" = none.
	SetNode(ctx context.Context, id, nodeID string) error
	SetOpen(ctx context.Context, id string, all bool) error
	Delete(ctx context.Context, id string) error
}

var placementsT = newTable[Placement]("placements", nil)

type placements struct{ crud[Placement] }

func (s placements) GetByName(ctx context.Context, name string) (Placement, error) {
	return s.one(ctx, "name = ?", name)
}

func (s placements) List(ctx context.Context) ([]Placement, error) { return s.many(ctx, "1 = 1") }

func (s placements) ListByNode(ctx context.Context, nodeID string) ([]Placement, error) {
	return s.many(ctx, "node_id = ?", nodeID)
}

func (s placements) SetNode(ctx context.Context, id, nodeID string) error {
	res, err := s.q.ExecContext(ctx, `UPDATE placements SET node_id = ? WHERE id = ?`, nodeID, id)
	return affected(res, err)
}

func (s placements) SetOpen(ctx context.Context, id string, all bool) error {
	res, err := s.q.ExecContext(ctx, `UPDATE placements SET open_to_all = ? WHERE id = ?`, all, id)
	return affected(res, err)
}

// PlacementOrgStore is the orgs each placement is open to.
type PlacementOrgStore interface {
	// Set replaces the placement's orgs. Call it inside a transaction.
	Set(ctx context.Context, placementID string, orgIDs []string) error
	OrgIDs(ctx context.Context, placementID string) ([]string, error)
	Has(ctx context.Context, placementID, orgID string) (bool, error)
}

type placementOrgs struct{ q querier }

func (s placementOrgs) Set(ctx context.Context, placementID string, orgIDs []string) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM placement_orgs WHERE placement_id = ?`, placementID); err != nil {
		return mapErr(err)
	}
	for _, o := range orgIDs {
		if _, err := s.q.ExecContext(ctx, `INSERT INTO placement_orgs (placement_id, org_id) VALUES (?, ?)`, placementID, o); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

func (s placementOrgs) OrgIDs(ctx context.Context, placementID string) ([]string, error) {
	var ids []string
	err := s.q.SelectContext(ctx, &ids, `SELECT org_id FROM placement_orgs WHERE placement_id = ? ORDER BY org_id`, placementID)
	return ids, mapErr(err)
}

func (s placementOrgs) Has(ctx context.Context, placementID, orgID string) (bool, error) {
	var n int
	err := s.q.GetContext(ctx, &n, `SELECT COUNT(*) FROM placement_orgs WHERE placement_id = ? AND org_id = ?`, placementID, orgID)
	return n > 0, mapErr(err)
}

// PlacementMove is a row of placement_moves: one repoint of a placement.
// FromNode and ToNode are node ids; Restore is a repoint off a forgotten node.
type PlacementMove struct {
	ID          string     `db:"id" json:"id"`
	PlacementID string     `db:"placement_id" json:"placement_id"`
	FromNode    string     `db:"from_node" json:"from_node"`
	ToNode      string     `db:"to_node" json:"to_node"`
	Restore     bool       `db:"restore" json:"restore"`
	State       string     `db:"state" json:"state"` // running | failed | done
	By          string     `db:"by" json:"by"`
	KeyID       string     `db:"key_id" json:"key_id"`
	StartedAt   time.Time  `db:"started_at" json:"started_at"`
	FinishedAt  *time.Time `db:"finished_at" json:"finished_at"`
	Error       string     `db:"error" json:"error"`
}

type PlacementMoveStore interface {
	Create(ctx context.Context, m PlacementMove) error
	Get(ctx context.Context, id string) (PlacementMove, error)
	// Latest is the placement's newest move; ErrNotFound when it has none.
	Latest(ctx context.Context, placementID string) (PlacementMove, error)
	// SetState writes the state, the error and the finish time only.
	SetState(ctx context.Context, id, state, errMsg string, finishedAt *time.Time) error
}

var placementMovesT = newTable[PlacementMove]("placement_moves", nil)

type placementMoves struct{ crud[PlacementMove] }

func (s placementMoves) Latest(ctx context.Context, placementID string) (PlacementMove, error) {
	return s.one(ctx, "placement_id = ? ORDER BY started_at DESC, rowid DESC LIMIT 1", placementID)
}

func (s placementMoves) SetState(ctx context.Context, id, state, errMsg string, finishedAt *time.Time) error {
	res, err := s.q.ExecContext(ctx, `UPDATE placement_moves SET state = ?, error = ?, finished_at = ? WHERE id = ?`,
		state, errMsg, finishedAt, id)
	return affected(res, err)
}
