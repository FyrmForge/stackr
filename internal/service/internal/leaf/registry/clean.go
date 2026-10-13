package registry

import (
	"context"
	"errors"
)

// InUse says whether an env runs, pins or can roll back to a version.
type InUse func(ctx context.Context, repo, digest string) (bool, error)

// Cleaned is what a Clean did.
type Cleaned struct {
	Deleted int
	// Freed is the manifests' sizes by their rows; the disk comes back at the
	// next garbage collect.
	Freed  int64
	Errors []string
}

// Clean is the mark half of the nightly cleanup: per repo, every version
// beyond the newest keep that inUse refuses, deleted as Delete does (manifest
// and row). A digest is deleted only when no tag on it is in use. One repo
// failing is noted in Errors and the rest go on. The sweep (gate pushes,
// garbage collect --delete-untagged, ungate) is the service's, since it
// needs Docker.
//
// Filled by G4.
func (l *Leaf) Clean(ctx context.Context, keep int, inUse InUse) (Cleaned, error) {
	return Cleaned{}, errors.ErrUnsupported
}
