package registry

import (
	"context"
	"errors"
	"time"
)

// Version is one image version of a repo: what the registry holds, with the
// detail of its row.
type Version struct {
	Tag       string
	Digest    string
	Platforms []string
	Size      int64
	CreatedAt time.Time
	// Source is "built", "imported" or "system"; "" for an image with no row
	// (pushed before records existed).
	Source      string
	UpstreamRef string
	CreatedBy   string
	// UsedBy names the envs that run it. The leaf cannot know; the caller
	// (service) fills it from the envs.
	UsedBy []string
}

// Versions are the repo's versions, newest first: the registry lists the tags
// and digests, the table adds detail. A row whose image is gone is not shown
// (and may be deleted here); an image with no row is shown with what the
// manifest says.
//
// Filled by G5.
func (l *Leaf) Versions(ctx context.Context, repo string) ([]Version, error) {
	return nil, errors.ErrUnsupported
}
