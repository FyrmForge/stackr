// Package imgcopy copies an image from an outside registry into the cluster
// registry, keeping only the CPU types the cluster has. It is a thin layer on
// go-containerregistry's remote package; crane's own filtered copy drops the
// index and so changes the digest, which this must not.
package imgcopy

import (
	"context"
	"errors"

	"github.com/google/go-containerregistry/pkg/authn"
)

// Result is what a copy left in the destination.
type Result struct {
	// Digest is the digest of what src's tag named: the index's for a
	// multi-arch image, never a child's. It equals the upstream digest, so
	// update checks against the upstream keep working.
	Digest string
	// Platforms are the CPU types now behind Digest in dst ("amd64",
	// "arm64"), including ones an earlier copy stored.
	Platforms []string
	// Size is the bytes of the stored platforms' config, layers and manifests.
	Size int64
}

// Copy copies src (a ref, "ghcr.io/foo/bar:1" or "postgres@sha256:...") to
// dst (a ref with a tag, "localhost:5000/<org>/ghcr.io/foo/bar:1") and
// returns what dst now holds. A nil auth is anonymous.
//
// The contract, which G1 fills in and everyone else relies on:
//   - platforms are CPU types ("amd64", "arm64"; linux only). An index is
//     copied with only the children for those platforms (config, layers and
//     manifest each), and the index bytes are then put unchanged, so the
//     digest stays the upstream one while the index lists platforms dst does
//     not hold (dst's registry has index validation off for this).
//   - A single-platform image is copied when its platform is wanted, else the
//     error names the platforms it has. A wanted platform src lacks is a clear
//     error naming what src has; nothing is half-copied.
//   - dst always gets a tag, the one in dst. Callers pass "sha256-<hex>" for a
//     digest-pinned src (regfront.ImportName), so the registry's untagged
//     garbage collect never takes a running image.
//   - Already-present blobs are skipped, so a second run for a new platform
//     copies only that platform.
//   - HEAD first: a HEAD on src does not count against Docker Hub's pull
//     limit. When dst already holds that digest with every wanted platform,
//     nothing is fetched.
//   - Credentials go only to the registry they were given for (srcAuth to
//     src's, dstAuth to dst's) and are never logged.
func Copy(ctx context.Context, src, dst string, srcAuth, dstAuth authn.Authenticator, platforms []string) (Result, error) {
	return Result{}, errors.ErrUnsupported
}
