// Package image owns images: what stackr built on this box, and the image
// watch cache (the registry's last answer per ref). One row per ref.
package image

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/git"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// LabelBuilt goes on every image the build flow makes; Cleanup prunes only
// those.
const LabelBuilt = "stackr.built"

// Grace keeps a fresh build out of Cleanup until a release can point at it.
// ponytail: time, not a lock; a cleanup racing a build older than an hour
// with no release yet would take it. Lock builds and cleanup if that bites.
const Grace = time.Hour

type Docker interface {
	PruneImages(ctx context.Context, labels map[string]string, keep []string) ([]string, error)
}

type Leaf struct {
	images store.ImageStore
	docker Docker
}

func New(images store.ImageStore, d Docker) *Leaf { return &Leaf{images: images, docker: d} }

func (l *Leaf) Get(ctx context.Context, id string) (store.Image, error) { return l.images.Get(ctx, id) }

func (l *Leaf) GetByRef(ctx context.Context, ref string) (store.Image, error) {
	return l.images.GetByRef(ctx, ref)
}

func (l *Leaf) List(ctx context.Context) ([]store.Image, error) { return l.images.List(ctx) }

// Name is the ref a build of this commit gets. A second build of one commit
// gets the job id appended, so it never overwrites the image an older
// release points at.
func (l *Leaf) Name(ctx context.Context, repoName, sha, jobID string) (string, error) {
	ref := git.ImageName(repoName, sha, jobID)
	_, err := l.images.GetByRef(ctx, ref)
	switch {
	case errors.Is(err, errs.ErrNotFound):
		return ref, nil
	case err != nil:
		return "", err
	}
	return ref + "-" + jobID[:min(8, len(jobID))], nil
}

// Built records a finished build; digest is the local image id.
func (l *Leaf) Built(ctx context.Context, ref, digest string) (store.Image, error) {
	now := time.Now().UTC()
	return l.upsert(ctx, ref, func(i *store.Image) { i.Digest, i.BuiltAt = digest, &now })
}

// Checked caches one registry answer. An error keeps the last good answer
// and retries next round; it never reads as an update.
func (l *Leaf) Checked(ctx context.Context, ref, digest, tag string, checkErr error) (store.Image, error) {
	now := time.Now().UTC()
	return l.upsert(ctx, ref, func(i *store.Image) {
		i.CheckedAt = &now
		if checkErr != nil {
			i.LastError = checkErr.Error()
			return
		}
		i.LastDigest, i.LastTag, i.LastError = digest, tag, ""
	})
}

func (l *Leaf) upsert(ctx context.Context, ref string, edit func(*store.Image)) (store.Image, error) {
	i, err := l.images.GetByRef(ctx, ref)
	if errors.Is(err, errs.ErrNotFound) {
		i = store.Image{ID: uuid.NewString(), Ref: ref, CreatedAt: time.Now().UTC()}
		edit(&i)
		return i, l.images.Create(ctx, i)
	}
	if err != nil {
		return i, err
	}
	edit(&i)
	return i, l.images.Update(ctx, i)
}

// Cleanup removes built images no release references. keep is every image
// id the release_tiles rows point at. Builds younger than Grace stay too.
// Watch-cache rows (never built here) are left alone. Returns the refs
// removed from the box.
func (l *Leaf) Cleanup(ctx context.Context, keep []string) ([]string, error) {
	all, err := l.images.List(ctx)
	if err != nil {
		return nil, err
	}
	var keepRefs []string
	var drop []store.Image
	cutoff := time.Now().Add(-Grace)
	for _, i := range all {
		switch {
		case i.BuiltAt == nil:
		case slices.Contains(keep, i.ID) || i.BuiltAt.After(cutoff):
			keepRefs = append(keepRefs, i.Ref, i.Digest)
		default:
			drop = append(drop, i)
		}
	}
	removed, err := l.docker.PruneImages(ctx, map[string]string{LabelBuilt: "true"}, keepRefs)
	if err != nil {
		return removed, err
	}
	for _, i := range drop {
		if err := l.images.Delete(ctx, i.ID); err != nil {
			return removed, err
		}
	}
	return removed, nil
}
