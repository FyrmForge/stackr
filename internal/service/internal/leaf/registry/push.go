package registry

import (
	"context"
	"errors"
)

// Push is a manifest push that has just succeeded at the registry front.
type Push struct {
	// OrgID is the pushing repo's org, "" for stackr's own (stackr.system).
	OrgID string
	Repo  string
	// Reference is what the manifest was put under: a tag, or a digest.
	Reference string
	// Digest is the registry's digest for it.
	Digest string
	// By is the user id for a developer push, the node id for a build.
	By string
}

// Pushed records a finished push (a build's or a developer's): it reads the
// manifest back from the registry for its platforms and size and Records the
// version as built. A push by digest records the digest under its sha256-<hex>
// tag. Called from regfront's Pushed callback, so it must be quick and its
// errors are only logged: a push is never failed for the record's sake.
//
// Filled by G2.
func (l *Leaf) Pushed(ctx context.Context, p Push) error {
	return errors.ErrUnsupported
}
