package store

import (
	"context"
	"time"
)

// IdempotencyKey is a row of idempotency_keys: the first answer to a request
// that carried an Idempotency-Key. ID is the hash of key, principal and route;
// RequestHash the hash of the body, so a reused key with another body is
// caught. Status 0 = the first request has not answered yet.
type IdempotencyKey struct {
	ID          string    `db:"id"`
	RequestHash string    `db:"request_hash"`
	Status      int       `db:"status"`
	Body        string    `db:"body"`
	CreatedAt   time.Time `db:"created_at"`
}

type IdempotencyKeyStore interface {
	// Create is errs.Conflict when the id is taken.
	Create(ctx context.Context, k IdempotencyKey) error
	Get(ctx context.Context, id string) (IdempotencyKey, error)
	Update(ctx context.Context, k IdempotencyKey) error
	Delete(ctx context.Context, id string) error
	// DeleteBefore prunes keys older than t.
	DeleteBefore(ctx context.Context, t time.Time) error
}

var idempotencyKeysT = newTable[IdempotencyKey]("idempotency_keys", nil)

type idempotencyKeys struct{ crud[IdempotencyKey] }

func (s idempotencyKeys) DeleteBefore(ctx context.Context, t time.Time) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_at < ?`, t)
	return err
}
