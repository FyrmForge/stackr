package store

import "context"

type actorKey struct{}

type actor struct{ user, key string }

// WithActor marks ctx with the user a verb acts for, and the API key behind
// the request ("" for a session); the jobs it queues and the audit events it
// writes remember them. The edge sets it once per request.
func WithActor(ctx context.Context, userID, keyID string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor{userID, keyID})
}

// ActorFrom is the user and key ctx acts for, "" for the system (a schedule,
// a push).
func ActorFrom(ctx context.Context) (userID, keyID string) {
	a, _ := ctx.Value(actorKey{}).(actor)
	return a.user, a.key
}
