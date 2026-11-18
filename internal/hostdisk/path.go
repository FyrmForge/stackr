package hostdisk

import "context"

// pathKind is a mounted folder somebody else prepared: probe, add, verify,
// folder, usage, drop, claim. The code is in ops.go and claim.go.
type pathKind struct{}

func (pathKind) run(ctx context.Context, e Env, r Request) Result {
	switch r.Op {
	case "probe":
		return probe(e, r)
	case "add":
		return add(e, r)
	case "verify":
		return verify(e, r)
	case "folder":
		return folder(e, r)
	case "usage":
		return usage(ctx, e, r)
	case "drop":
		return drop(e, r)
	case "claim":
		return claim(e, r)
	}
	return fail(CodeUnsupported, "op "+r.Op)
}
