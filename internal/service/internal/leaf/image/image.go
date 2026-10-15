// Package image owns images: what stackr built on this box, and the image
// watch cache (the registry's last answer per ref). One row per ref.
package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
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
	PruneImages(ctx context.Context, labels map[string]string, keep []string) ([]string, int64, error)
	Pull(ctx context.Context, ref, auth string, log io.Writer) error
	LocalDigest(ctx context.Context, ref string) (string, error)
	ListImages(ctx context.Context, labels map[string]string) ([]docker.Image, error)
	EnsureBuilder(ctx context.Context, name string, memMB int) error
	Build(
		ctx context.Context,
		builder, dir, dockerfile, tag string,
		buildArgs, labels map[string]string,
		log io.Writer,
	) (string, error)
}

// Builder is the one buildx builder every build shares.
const Builder = "stackr-builder"

type Leaf struct {
	images store.ImageStore
	docker Docker
}

func New(images store.ImageStore, d Docker) *Leaf {
	return &Leaf{images: images, docker: d}
}

func (l *Leaf) Get(ctx context.Context, id string) (store.Image, error) {
	return l.images.Get(ctx, id)
}

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

// ErrCleanedUp: a build of ours whose Docker image Cleanup removed; the
// caller names the tile.
var ErrCleanedUp = errors.New("the image was cleaned up; rebuild it")

// ClusterRegistry prefixes the refs of builds in the cluster registry; every
// node has them loaded from the registry on the controller under this name.
const ClusterRegistry = "localhost:" + installspec.RegistryPort + "/"

// Ensure makes ref present on the box and returns its registry digest ("" for
// an image built here). A build of ours is loaded from the cluster registry
// only when this node lacks it: the build itself (the image id the build
// recorded) or the registry's copy (LocalDigest checks the id); an older one,
// never pushed, cannot be. A digest-pinned ref already here is not pulled (it
// cannot have moved); a tag is pulled every time, so a first deploy gets what
// the tag means now. auth is the X-Registry-Auth blob, "" = anonymous.
func (l *Leaf) Ensure(ctx context.Context, ref, auth string, log io.Writer) (string, error) {
	if i, err := l.images.GetByRef(ctx, ref); err == nil && i.BuiltAt != nil {
		if l.isBuild(ctx, ref, i.Digest) {
			return "", nil
		}
		_, err := l.docker.LocalDigest(ctx, ref)
		switch {
		case err == nil:
			return "", nil
		case !errors.Is(err, docker.ErrNotFound):
			return "", err
		case !strings.HasPrefix(ref, ClusterRegistry):
			return "", fmt.Errorf("%s: %w", ref, ErrCleanedUp)
		}
		if err := l.docker.Pull(ctx, ref, "", log); err != nil {
			return "", err
		}
		return "", nil
	}
	if strings.Contains(ref, "@sha256:") {
		if d, err := l.docker.LocalDigest(ctx, ref); err == nil && d != "" {
			return d, nil
		}
	}
	if err := l.docker.Pull(ctx, ref, auth, log); err != nil {
		return "", err
	}
	return l.docker.LocalDigest(ctx, ref)
}

// isBuild reports whether ref here is the very image the build made (its
// node, before any load): the image id is the content's own digest.
func (l *Leaf) isBuild(ctx context.Context, ref, id string) bool {
	if id == "" {
		return false
	}
	ims, err := l.docker.ListImages(ctx, map[string]string{LabelBuilt: "true"})
	if err != nil {
		return false
	}
	for _, im := range ims {
		if im.ID == id && slices.Contains(im.Tags, ref) {
			return true
		}
	}
	return false
}

// Build builds dir into ref and records it. The label lets Cleanup find it.
func (l *Leaf) Build(
	ctx context.Context,
	dir, dockerfile, ref string,
	args map[string]string,
	log io.Writer,
) (store.Image, error) {
	if err := l.docker.EnsureBuilder(ctx, Builder, 0); err != nil {
		return store.Image{}, err
	}
	id, err := l.docker.Build(ctx, Builder, dir, dockerfile, ref, args, map[string]string{LabelBuilt: "true"}, log)
	if err != nil {
		return store.Image{}, err
	}
	return l.Built(ctx, ref, id)
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

// Plan is what a cleanup sweep decided before any node is touched: the refs
// every node's prune must keep, and the rows whose images are candidates to go.
type Plan struct {
	Keep []string
	drop []store.Image
}

// Plan decides which built images a sweep may remove. keep is the image ids
// to keep; builds younger than Grace stay too. Watch-cache rows (never built
// here) are left alone. extra are more refs or ids to keep (what containers
// use, on any node).
func (l *Leaf) Plan(ctx context.Context, keep, extra []string) (Plan, error) {
	all, err := l.images.List(ctx)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Keep: slices.Clone(extra)}
	cutoff := time.Now().Add(-Grace)
	for _, i := range all {
		switch {
		case i.BuiltAt == nil:
		case slices.Contains(keep, i.ID) || slices.Contains(extra, i.Ref) || slices.Contains(extra, i.Digest) ||
			i.BuiltAt.After(cutoff):
			p.Keep = append(p.Keep, i.Ref, i.Digest)
		default:
			p.drop = append(p.drop, i)
		}
	}
	return p, nil
}

// Prune removes the unkept built images from the node ctx names. Run it on
// every node before Settle: a row may go only once its image has.
func (l *Leaf) Prune(ctx context.Context, p Plan) (removed []string, freed int64, err error) {
	return l.docker.PruneImages(ctx, map[string]string{LabelBuilt: "true"}, p.Keep)
}

// Settle deletes the rows of the images Prune removed. A row goes only when
// its Docker image did and no release still pins it (pinned is every id any
// release references): the row is what the release's pin points at. removed
// is the ids every node's Prune reported, a partial failure's included.
func (l *Leaf) Settle(ctx context.Context, p Plan, pinned, removed []string) error {
	for _, i := range p.drop {
		if slices.Contains(pinned, i.ID) || !slices.Contains(removed, i.Digest) {
			continue
		}
		if err := l.images.Delete(ctx, i.ID); err != nil {
			return err
		}
	}
	return nil
}

// Cleanup is Plan, Prune and Settle on the one node ctx names: the images
// removed from the box and the bytes that freed (up to: layers can be
// shared). A partial failure still reports what went: its rows go too.
func (l *Leaf) Cleanup(ctx context.Context, keep, pinned, extra []string) ([]string, int64, error) {
	p, err := l.Plan(ctx, keep, extra)
	if err != nil {
		return nil, 0, err
	}
	removed, freed, pruneErr := l.Prune(ctx, p)
	if err := l.Settle(ctx, p, pinned, removed); err != nil {
		return removed, freed, errors.Join(pruneErr, err)
	}
	return removed, freed, pruneErr
}
