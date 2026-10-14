package store

import (
	"context"
	"time"
)

// JoinToken is a row of join_tokens. Hash is the SHA-256 of the token; the
// token itself is shown once, at mint. UsedAt is set by the one join request
// that spends it, which also fills NodeID, CSR, WGPublicKey and Facts (a JSON
// document the node reported). CertPEM is set at confirm, RejectedAt at reject.
type JoinToken struct {
	ID          string     `db:"id" json:"id"`
	Hash        string     `db:"hash" json:"-"`
	NodeName    string     `db:"node_name" json:"node_name"`
	Tags        string     `db:"tags" json:"tags"`
	CreatedBy   string     `db:"created_by" json:"created_by"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`
	ExpiresAt   time.Time  `db:"expires_at" json:"expires_at"`
	UsedAt      *time.Time `db:"used_at" json:"used_at"`
	NodeID      string     `db:"node_id" json:"node_id"`
	CSR         string     `db:"csr" json:"-"`
	WGPublicKey string     `db:"wg_public_key" json:"wg_public_key"`
	Facts       string     `db:"facts" json:"-"`
	CertPEM     string     `db:"cert_pem" json:"-"`
	RejectedAt  *time.Time `db:"rejected_at" json:"rejected_at"`
}

type JoinTokenStore interface {
	Create(ctx context.Context, t JoinToken) error
	Get(ctx context.Context, id string) (JoinToken, error)
	Update(ctx context.Context, t JoinToken) error
	GetByHash(ctx context.Context, hash string) (JoinToken, error)
	GetByNode(ctx context.Context, nodeID string) (JoinToken, error)
	// Spend stamps used_at on an unused, unexpired token and reports
	// ErrNotFound when there is none, so a token is spent exactly once even
	// when two requests race.
	Spend(ctx context.Context, hash string, now time.Time) error
	// Used is every spent token, oldest first.
	Used(ctx context.Context) ([]JoinToken, error)
	// ExpireUnspent ends every token not yet spent and returns how many.
	ExpireUnspent(ctx context.Context, now time.Time) (int, error)
	// ExpireUnspentFor ends the tokens for one node name not yet spent.
	ExpireUnspentFor(ctx context.Context, nodeName string, now time.Time) (int, error)
}

var joinTokensT = newTable[JoinToken]("join_tokens", nil)

type joinTokens struct{ crud[JoinToken] }

func (s joinTokens) GetByHash(ctx context.Context, hash string) (JoinToken, error) {
	return s.one(ctx, "hash = ?", hash)
}

func (s joinTokens) GetByNode(ctx context.Context, nodeID string) (JoinToken, error) {
	return s.one(ctx, "node_id = ? ORDER BY used_at DESC", nodeID) // newest: a rejoin leaves older rows
}

func (s joinTokens) Spend(ctx context.Context, hash string, now time.Time) error {
	res, err := s.q.ExecContext(ctx,
		"UPDATE join_tokens SET used_at = ? WHERE hash = ? AND used_at IS NULL AND expires_at > ?", now, hash, now)
	return affected(res, err)
}

func (s joinTokens) Used(ctx context.Context) ([]JoinToken, error) {
	return s.many(ctx, "used_at IS NOT NULL ORDER BY used_at")
}

func (s joinTokens) ExpireUnspentFor(ctx context.Context, nodeName string, now time.Time) (int, error) {
	res, err := s.q.ExecContext(ctx, "UPDATE join_tokens SET expires_at = ? WHERE node_name = ? AND used_at IS NULL AND expires_at > ?", now, nodeName, now)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s joinTokens) ExpireUnspent(ctx context.Context, now time.Time) (int, error) {
	res, err := s.q.ExecContext(ctx, "UPDATE join_tokens SET expires_at = ? WHERE used_at IS NULL AND expires_at > ?", now, now)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
