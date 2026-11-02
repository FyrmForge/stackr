package store

import (
	"context"
	"time"
)

// APIKey is a row of api_keys. OrgID nil = unbound, the account's key (stackr login).
type APIKey struct {
	ID        string    `db:"id" json:"id"`
	UserID    string    `db:"user_id" json:"user_id"`
	OrgID     *string   `db:"org_id" json:"org_id"`
	Name      string    `db:"name" json:"name"`
	TokenHash string    `db:"token_hash" json:"-"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	// Level is the key's ceiling: "" none, else viewer|member|owner|admin.
	Level string `db:"level" json:"level"`
	// StackID narrows the key to one stack; Stack is its slug, filled on list.
	StackID *string `db:"stack_id" json:"-"`
	Stack   string  `db:"-" json:"stack"`
	// ExpiresAt nil = never. LastUsedAt is stamped by a request the key
	// opens, at most once a minute.
	ExpiresAt  *time.Time `db:"expires_at" json:"expires_at"`
	LastUsedAt *time.Time `db:"last_used_at" json:"last_used_at"`
	// Owner is the account's email, filled on the org and admin lists.
	Owner string `db:"-" json:"owner,omitempty"`
}

type APIKeyStore interface {
	Create(ctx context.Context, k APIKey) error
	Get(ctx context.Context, id string) (APIKey, error)
	GetByHash(ctx context.Context, hash string) (APIKey, error)
	ListByUser(ctx context.Context, userID string) ([]APIKey, error)
	// ListByOrg is the keys bound to an org; List is every key.
	ListByOrg(ctx context.Context, orgID string) ([]APIKey, error)
	List(ctx context.Context) ([]APIKey, error)
	// Touch stamps last_used_at unless it is already within the last minute.
	Touch(ctx context.Context, id string, now time.Time) error
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

func (s apiKeys) ListByOrg(ctx context.Context, orgID string) ([]APIKey, error) {
	return s.many(ctx, "org_id = ? ORDER BY created_at", orgID)
}

func (s apiKeys) List(ctx context.Context) ([]APIKey, error) {
	return s.many(ctx, "1 = 1 ORDER BY created_at")
}

func (s apiKeys) Touch(ctx context.Context, id string, now time.Time) error {
	_, err := s.q.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = ? WHERE id = ? AND (last_used_at IS NULL OR last_used_at < ?)`,
		now, id, now.Add(-time.Minute))
	return err
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
