package store

import (
	"context"
	"time"
)

// NodeCert is a row of node_certs: one cert the cluster CA issued. NodeID is
// empty for the controller cert. RevokedAt is nil until revoked.
type NodeCert struct {
	Serial   string    `db:"serial" json:"serial"`
	NodeID   string    `db:"node_id" json:"node_id"`
	Role     string    `db:"role" json:"role"`
	NotAfter time.Time `db:"not_after" json:"not_after"`
	// Issuer is the fingerprint of the root that signed it, "" for certs
	// from before root rollovers.
	Issuer    string     `db:"issuer" json:"issuer"`
	RevokedAt *time.Time `db:"revoked_at" json:"revoked_at"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
}

type NodeCertStore interface {
	Create(ctx context.Context, c NodeCert) error
	Get(ctx context.Context, serial string) (NodeCert, error)
	ListByNode(ctx context.Context, nodeID string) ([]NodeCert, error)
	// ListLive is every unrevoked cert, oldest first: a rollover reads the
	// issuers of what nodes hold right now.
	ListLive(ctx context.Context) ([]NodeCert, error)
	// Revoke stamps one cert; ErrNotFound when the serial is unknown.
	Revoke(ctx context.Context, serial string, at time.Time) error
	// RevokeNode stamps every live cert of a node and returns how many.
	RevokeNode(ctx context.Context, nodeID string, at time.Time) (int, error)
	// IsRevoked is true for a revoked serial. An unknown serial is not
	// revoked: the CA signature already vouches for it.
	IsRevoked(ctx context.Context, serial string) (bool, error)
}

// nodeCerts is keyed by serial, not id, so it does not use crud.
type nodeCerts struct{ q querier }

// NewNodeCerts binds the store to a querier. Tables does not wire it yet.
func NewNodeCerts(q querier) NodeCertStore { return nodeCerts{q} }

const nodeCertCols = "serial, node_id, role, not_after, issuer, revoked_at, created_at"

func (s nodeCerts) Create(ctx context.Context, c NodeCert) error {
	_, err := s.q.NamedExecContext(ctx, "INSERT INTO node_certs ("+nodeCertCols+
		") VALUES (:serial, :node_id, :role, :not_after, :issuer, :revoked_at, :created_at)", c)
	return mapErr(err)
}

func (s nodeCerts) Get(ctx context.Context, serial string) (NodeCert, error) {
	var c NodeCert
	err := s.q.GetContext(ctx, &c, "SELECT "+nodeCertCols+" FROM node_certs WHERE serial = ?", serial)
	return c, mapErr(err)
}

func (s nodeCerts) ListByNode(ctx context.Context, nodeID string) ([]NodeCert, error) {
	var rows []NodeCert
	err := s.q.SelectContext(ctx, &rows, "SELECT "+nodeCertCols+
		" FROM node_certs WHERE node_id = ? ORDER BY created_at, serial", nodeID)
	return rows, mapErr(err)
}

func (s nodeCerts) ListLive(ctx context.Context) ([]NodeCert, error) {
	var rows []NodeCert
	err := s.q.SelectContext(ctx, &rows, "SELECT "+nodeCertCols+
		" FROM node_certs WHERE revoked_at IS NULL ORDER BY created_at, serial")
	return rows, mapErr(err)
}

func (s nodeCerts) Revoke(ctx context.Context, serial string, at time.Time) error {
	res, err := s.q.ExecContext(ctx,
		"UPDATE node_certs SET revoked_at = COALESCE(revoked_at, ?) WHERE serial = ?", at, serial)
	return affected(res, err)
}

func (s nodeCerts) RevokeNode(ctx context.Context, nodeID string, at time.Time) (int, error) {
	res, err := s.q.ExecContext(ctx,
		"UPDATE node_certs SET revoked_at = ? WHERE node_id = ? AND revoked_at IS NULL", at, nodeID)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s nodeCerts) IsRevoked(ctx context.Context, serial string) (bool, error) {
	var n int
	err := s.q.GetContext(ctx, &n,
		"SELECT COUNT(*) FROM node_certs WHERE serial = ? AND revoked_at IS NOT NULL", serial)
	return n > 0, mapErr(err)
}
