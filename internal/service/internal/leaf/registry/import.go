package registry

import (
	"context"
	"errors"

	"github.com/google/go-containerregistry/pkg/authn"
)

// ImportRequest is one outside image to bring into the registry.
type ImportRequest struct {
	OrgID, OrgSlug string
	// Ref is the upstream ref as the user wrote it ("postgres:16",
	// "ghcr.io/foo/bar@sha256:...").
	Ref string
	// Auth is the org's credential for Ref's registry, nil = anonymous. It
	// stays on the controller and goes only to Ref's registry.
	Auth authn.Authenticator
	// Platforms are the CPU types to hold ("amd64", "arm64"): every node's.
	Platforms []string
	// By is the user id who caused it, "" for the system.
	By string
}

// Imported is what an import left behind.
type Imported struct {
	// LocalRef is what a node pulls: Local(repo, tag) of the import repo.
	LocalRef string
	// Digest is the upstream digest, which the copy keeps.
	Digest string
}

// Import copies req.Ref into the registry under regfront.ImportName(OrgSlug,
// Ref), by imgcopy.Copy from the upstream to the loopback registry, and
// Records the version (source imported, upstream_ref req.Ref, the platforms
// and size Copy reports) in the same step. Calling it again for a ref that is
// there with these platforms fetches nothing and records nothing new; a new
// platform adds only that platform and updates the row. Waits while the
// cleanup sweep gates the registry. Pull and registry errors come back
// wrapped, never with the credential in them.
//
// Filled by G2.
func (l *Leaf) Import(ctx context.Context, req ImportRequest) (Imported, error) {
	return Imported{}, errors.ErrUnsupported
}
