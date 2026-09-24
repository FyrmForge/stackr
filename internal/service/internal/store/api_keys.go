package store

import (
	"context"
	"time"
)

// APIKey is a row of api_keys. OrgID nil = unbound (admin-only, DECIDE 13).
type APIKey struct {
	ID        string    `db:"id"`
	UserID    string    `db:"user_id"`
	OrgID     *string   `db:"org_id"`
	Name      string    `db:"name"`
	TokenHash string    `db:"token_hash"`
	CreatedAt time.Time `db:"created_at"`
}

type APIKeyStore interface {
	Create(ctx context.Context, k APIKey) error
	Get(ctx context.Context, id string) (APIKey, error)
	GetByHash(ctx context.Context, hash string) (APIKey, error)
	ListByUser(ctx context.Context, userID string) ([]APIKey, error)
	Update(ctx context.Context, k APIKey) error
	Delete(ctx context.Context, id string) error
	// DeleteByUser removes every key of a user; orgID non-nil narrows it to
	// the keys bound to that org.
	DeleteByUser(ctx context.Context, userID string, orgID *string) error
}

var apiKeysT = newTable[APIKey]("api_keys", nil)

type apiKeys struct{ crud[APIKey] }

func (s apiKeys) GetByHash(ctx context.Context, hash string) (APIKey, error) {
	return s.one(ctx, "token_hash = ?", hash)
}

func (s apiKeys) ListByUser(ctx context.Context, userID string) ([]APIKey, error) {
	return s.many(ctx, "user_id = ?", userID)
}

func (s apiKeys) DeleteByUser(ctx context.Context, userID string, orgID *string) error {
	var err error
	if orgID == nil {
		_, err = s.q.ExecContext(ctx, `DELETE FROM api_keys WHERE user_id = ?`, userID)
	} else {
		_, err = s.q.ExecContext(ctx, `DELETE FROM api_keys WHERE user_id = ? AND org_id = ?`, userID, *orgID)
	}
	return err
}
