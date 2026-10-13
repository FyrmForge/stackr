package registry

import (
	"context"
	"errors"
)

// Delete removes one version: the manifest by digest at the registry (which
// drops every tag on it) and its row, together. Deleting what is already gone
// is not an error. Whether an env still uses the version is not decided here:
// the service checks that and warns first. The blobs stay until the next
// garbage collect.
//
// Filled by G5.
func (l *Leaf) Delete(ctx context.Context, repo, digest string) error {
	return errors.ErrUnsupported
}
