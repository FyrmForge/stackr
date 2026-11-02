package service

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// idempotencyKeep is how long the first answer to an Idempotency-Key is
// replayed.
const idempotencyKeep = 24 * time.Hour

// WithActor marks ctx with the user a request acts for: the jobs it queues
// record them (Job.ActorID).
func WithActor(ctx context.Context, userID string) context.Context {
	return store.WithActor(ctx, userID)
}

// Answer is a request's first response, kept to replay.
type Answer struct {
	Status int
	Body   []byte
}

// IdempotencyBegin claims id (the hash of key, principal and route) for the
// request whose body hashes to reqHash. A key already answered returns that
// answer to replay; one still running is a conflict; one reused with another
// body is invalid. A nil answer means the caller owns the claim: it ends in
// IdempotencyFinish or IdempotencyRelease.
func (o *Orchestrator) IdempotencyBegin(ctx context.Context, id, reqHash string) (*Answer, error) {
	now := time.Now().UTC()
	if err := o.store.IdempotencyKeys.DeleteBefore(ctx, now.Add(-idempotencyKeep)); err != nil {
		return nil, err
	}
	err := o.store.IdempotencyKeys.Create(ctx, store.IdempotencyKey{ID: id, RequestHash: reqHash, CreatedAt: now})
	if _, taken := errs.IsConflict(err); !taken {
		return nil, err
	}
	k, err := o.store.IdempotencyKeys.Get(ctx, id)
	switch {
	case err != nil:
		return nil, err
	case k.RequestHash != reqHash:
		return nil, errs.Invalidf("Idempotency-Key", "this key was used for a different request")
	case k.Status == 0:
		return nil, errs.Conflictf("a request with this Idempotency-Key is still running")
	}
	return &Answer{Status: k.Status, Body: []byte(k.Body)}, nil
}

// IdempotencyFinish keeps the first answer under id.
func (o *Orchestrator) IdempotencyFinish(ctx context.Context, id, reqHash string, a Answer) error {
	return o.store.IdempotencyKeys.Update(ctx, store.IdempotencyKey{
		ID: id, RequestHash: reqHash, Status: a.Status, Body: string(a.Body), CreatedAt: time.Now().UTC(),
	})
}

// IdempotencyRelease drops a claim whose request failed, so a retry runs.
func (o *Orchestrator) IdempotencyRelease(ctx context.Context, id string) error {
	return o.store.IdempotencyKeys.Delete(ctx, id)
}
