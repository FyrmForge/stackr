package hostdisk

import "context"

// kind is one disk kind: it answers the ops of its Requests. Run picks the
// kind by Request.Kind.
type kind interface {
	run(ctx context.Context, e Env, r Request) Result
}

var kinds = map[string]kind{"path": pathKind{}, "xfs": xfsKind{}, "paneldb": dbKind{}}
