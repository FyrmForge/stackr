package store

import (
	"context"
	"time"
)

// RegistryToken is a row of registry_tokens: one token the registry front
// issued. Kind is node | builder | user | admin; Scope the granted access as
// space-separated "repo:action,action" entries.
type RegistryToken struct {
	ID        string    `db:"id" json:"id"`
	Kind      string    `db:"kind" json:"kind"`
	SubjectID string    `db:"subject_id" json:"subject_id"`
	Scope     string    `db:"scope" json:"scope"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	ExpiresAt time.Time `db:"expires_at" json:"expires_at"`
}

type RegistryTokenStore interface {
	Create(ctx context.Context, t RegistryToken) error
	Get(ctx context.Context, id string) (RegistryToken, error)
	// ListBySubject is newest first.
	ListBySubject(ctx context.Context, subjectID string, limit int) ([]RegistryToken, error)
	// DeleteExpiredBefore prunes the audit trail.
	DeleteExpiredBefore(ctx context.Context, t time.Time) error
}

var registryTokensT = newTable[RegistryToken]("registry_tokens", nil)

type registryTokens struct{ crud[RegistryToken] }

func (s registryTokens) ListBySubject(ctx context.Context, subjectID string, limit int) ([]RegistryToken, error) {
	return s.many(ctx, "subject_id = ? ORDER BY created_at DESC LIMIT ?", subjectID, limit)
}

func (s registryTokens) DeleteExpiredBefore(ctx context.Context, t time.Time) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM registry_tokens WHERE expires_at < ?`, t)
	return err
}
