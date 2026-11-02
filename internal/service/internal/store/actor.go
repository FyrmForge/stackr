package store

import "context"

type actorKey struct{}

// WithActor marks ctx with the user a verb acts for; the jobs it queues
// remember them. The edge sets it once per request.
func WithActor(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, actorKey{}, userID)
}

// ActorFrom is the user ctx acts for, "" for the system (a schedule, a push).
func ActorFrom(ctx context.Context) string {
	s, _ := ctx.Value(actorKey{}).(string)
	return s
}
