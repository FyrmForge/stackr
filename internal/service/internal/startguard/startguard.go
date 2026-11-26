// Package startguard carries a check that a job's flow runs right before it
// starts a container: the job's own gate ran at its start, but a deploy, a
// promote or a run can spend minutes preparing, and the disk may fail in them.
package startguard

import "context"

type key struct{}

// With returns ctx with guard in it. A nil guard changes nothing.
func With(ctx context.Context, guard func() error) context.Context {
	if guard == nil {
		return ctx
	}
	return context.WithValue(ctx, key{}, guard)
}

// Check runs the guard of ctx, if it has one.
func Check(ctx context.Context) error {
	if g, ok := ctx.Value(key{}).(func() error); ok {
		return g()
	}
	return nil
}
