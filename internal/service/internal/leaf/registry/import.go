package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/FyrmForge/stackr/internal/service/internal/imgcopy"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
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
	// Log gets the warning when an unreachable upstream is answered from our
	// own copy; nil = none.
	Log io.Writer
}

// Imported is what an import left behind.
type Imported struct {
	// LocalRef is what a node pulls: Local(repo, tag) of the import repo.
	LocalRef string
	// Digest is the upstream digest, which the copy keeps.
	Digest string
}

// copier is imgcopy.Copy's shape, so a test can stand in for the network.
type copier func(ctx context.Context, src, dst string, srcAuth, dstAuth authn.Authenticator, platforms []string) (imgcopy.Result, error)

// WithCopier replaces the copy (tests).
func (l *Leaf) WithCopier(c copier) *Leaf {
	l.copy = c
	return l
}

// Import copies req.Ref into the registry under regfront.ImportName(OrgSlug,
// Ref), by imgcopy.Copy from the upstream to the loopback registry, and
// Records the version (source imported, upstream_ref req.Ref, the platforms
// and size Copy reports) in the same step. Calling it again for a ref that is
// there with these platforms fetches nothing and records nothing new; a new
// platform adds only that platform and updates the row. Waits while the
// cleanup sweep gates the registry. Pull and registry errors come back
// wrapped, never with the credential in them.
func (l *Leaf) Import(ctx context.Context, req ImportRequest) (Imported, error) {
	if len(req.Platforms) == 0 {
		return Imported{}, fmt.Errorf("import %s: no platforms to hold", req.Ref)
	}
	leave, err := l.Hold(ctx)
	if err != nil {
		return Imported{}, err
	}
	defer leave()
	repo, tag := regfront.ImportName(req.OrgSlug, req.Ref)
	dst := strings.TrimPrefix(l.base, "http://") + "/" + repo + ":" + tag
	res, err := l.copy(ctx, req.Ref, dst, req.Auth, nil, req.Platforms)
	var down *imgcopy.UnreachableError
	if errors.As(err, &down) && ctx.Err() == nil {
		// The upstream is down, not saying no: a tag we already hold in full
		// still runs. ponytail: any held copy, however old; a max age if it
		// ever matters.
		if held, ok, herr := imgcopy.Held(ctx, dst, nil, req.Platforms); herr == nil && ok {
			age := "an earlier import"
			if row, gerr := l.images.GetByRepoDigest(ctx, repo, held.Digest); gerr == nil {
				age = sinceText(time.Since(row.CreatedAt)) + " ago"
			}
			if req.Log != nil {
				_, _ = fmt.Fprintf(req.Log, "could not reach %s (%v); using the copy from %s, it may not be the newest\n", down.Registry, down.Err, age)
			}
			return Imported{LocalRef: l.Local(repo, tag), Digest: held.Digest}, nil
		}
	}
	if err != nil {
		return Imported{}, fmt.Errorf("import %s: %w", req.Ref, err)
	}
	org := req.OrgID
	err = l.Record(ctx, store.RegistryImage{
		OrgID: &org, Repo: repo, Tag: tag, Digest: res.Digest,
		Platforms: JoinPlatforms(res.Platforms), Source: SourceImported,
		UpstreamRef: req.Ref, Size: res.Size, CreatedBy: req.By,
	})
	if err != nil {
		return Imported{}, fmt.Errorf("import %s: record: %w", req.Ref, err)
	}
	return Imported{LocalRef: l.Local(repo, tag), Digest: res.Digest}, nil
}

// sinceText is a coarse age: "5 minutes", "3 hours", "2 days".
func sinceText(d time.Duration) string {
	unit, n := "minute", int(d/time.Minute)
	switch {
	case d >= 48*time.Hour:
		unit, n = "day", int(d/(24*time.Hour))
	case d >= time.Hour:
		unit, n = "hour", int(d/time.Hour)
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s", n, unit)
}
