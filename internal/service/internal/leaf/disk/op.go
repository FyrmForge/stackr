package disk

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The two states that carry an unfinished operation. Both are "not ready":
// every ordinary caller is refused, only the job that owns the operation,
// under WithOp, sees through them.
const (
	StateAdopting   = "adopting"
	StateForgetting = "forgetting"
)

type opKey struct{}

type opVal struct{ diskID, mode string }

// WithOp is the context of the job that owns an operation on disk diskID.
// mode is StateAdopting (the disk answers as ready) or StateForgetting (it
// answers as absent). It is honoured only while the row is in that very
// state, and only for that disk. Never store it or hand it to a queued job.
func WithOp(ctx context.Context, diskID, mode string) context.Context {
	return context.WithValue(ctx, opKey{}, opVal{diskID, mode})
}

// WithoutOp drops an operation the context carries.
func WithoutOp(ctx context.Context) context.Context {
	return context.WithValue(ctx, opKey{}, opVal{})
}

// OpOf is the operation ctx runs on d: StateAdopting or StateForgetting, or
// ok false when there is none for this disk in its current state.
func OpOf(ctx context.Context, d store.NodeDisk) (mode string, ok bool) {
	o, _ := ctx.Value(opKey{}).(opVal)
	if o.diskID == "" || o.diskID != d.ID || o.mode != d.State {
		return "", false
	}
	return o.mode, o.mode == StateAdopting || o.mode == StateForgetting
}
