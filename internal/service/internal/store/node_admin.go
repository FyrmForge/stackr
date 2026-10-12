package store

import (
	"context"
	"time"
)

// ForgottenNode is a node that was forgotten: its envs still carry its id.
type ForgottenNode struct {
	ID          string    `db:"id" json:"id"`
	Name        string    `db:"name" json:"name"`
	ForgottenAt time.Time `db:"forgotten_at" json:"forgotten_at"`
}

// NodeOrgStore is the orgs each node is opened to.
type NodeOrgStore interface {
	// OrgIDs is the orgs one node is opened to.
	OrgIDs(ctx context.Context, nodeID string) ([]string, error)
	// Set replaces the node's orgs. Call it inside a transaction.
	Set(ctx context.Context, nodeID string, orgIDs []string) error
	// Has is true when the node is opened to the org.
	Has(ctx context.Context, nodeID, orgID string) (bool, error)
}

type nodeOrgs struct{ q querier }

func (s nodeOrgs) OrgIDs(ctx context.Context, nodeID string) ([]string, error) {
	var ids []string
	err := s.q.SelectContext(ctx, &ids, `SELECT org_id FROM node_orgs WHERE node_id = ? ORDER BY org_id`, nodeID)
	return ids, mapErr(err)
}

func (s nodeOrgs) Set(ctx context.Context, nodeID string, orgIDs []string) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM node_orgs WHERE node_id = ?`, nodeID); err != nil {
		return mapErr(err)
	}
	for _, o := range orgIDs {
		if _, err := s.q.ExecContext(ctx, `INSERT INTO node_orgs (node_id, org_id) VALUES (?, ?)`, nodeID, o); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

func (s nodeOrgs) Has(ctx context.Context, nodeID, orgID string) (bool, error) {
	var n int
	err := s.q.GetContext(ctx, &n, `SELECT COUNT(*) FROM node_orgs WHERE node_id = ? AND org_id = ?`, nodeID, orgID)
	return n > 0, mapErr(err)
}

type ForgottenNodeStore interface {
	Create(ctx context.Context, f ForgottenNode) error
	List(ctx context.Context) ([]ForgottenNode, error)
}

type forgottenNodes struct{ q querier }

func (s forgottenNodes) Create(ctx context.Context, f ForgottenNode) error {
	_, err := s.q.NamedExecContext(ctx, `INSERT INTO forgotten_nodes (id, name, forgotten_at) VALUES (:id, :name, :forgotten_at)`, f)
	return mapErr(err)
}

func (s forgottenNodes) List(ctx context.Context) ([]ForgottenNode, error) {
	var rows []ForgottenNode
	err := s.q.SelectContext(ctx, &rows, `SELECT id, name, forgotten_at FROM forgotten_nodes ORDER BY forgotten_at`)
	return rows, mapErr(err)
}
