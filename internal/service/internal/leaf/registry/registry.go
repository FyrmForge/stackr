// Package registry owns the cluster registry: the images in it and the
// registry_images row kept for each. It is the only holder of the registry's
// address and of that table, and the only code that talks to the registry;
// import, push record, delete, cleanup and listing all go through it and
// update the table in the same step. The registry decides what exists; a row
// adds detail, and a row whose image is gone is not shown.
//
// Files are split by operation so the workers that fill them do not meet:
// import.go and push.go (Import, Pushed), list.go (List), versions.go and
// delete.go (Versions, Delete), clean.go (Clean). This file is the shared
// part: the type, the address and the one row writer, Record.
package registry

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/imgcopy"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Sources of a version, the registry_images.source column.
const (
	SourceBuilt    = "built"
	SourceImported = "imported"
	SourceSystem   = "system"
)

type Leaf struct {
	images store.RegistryImageStore
	// base is the registry's HTTP address on the head's loopback.
	base string
	// copy and gate are import.go's: the filtered copy (imgcopy.Copy) and the
	// cleanup sweep's wait.
	copy copier
	gate func(context.Context) error
}

func New(images store.RegistryImageStore) *Leaf {
	return &Leaf{images: images, base: "http://127.0.0.1:" + installspec.RegistryPort, copy: imgcopy.Copy}
}

// WithBase points the leaf at another registry address (tests).
func (l *Leaf) WithBase(base string) *Leaf {
	l.base = base
	return l
}

// Local is the ref the controller and every node pull a repo and tag by:
// the registry on the node's own loopback, which on a worker is the tunnel to
// the head. Nothing outside this leaf builds one by hand.
func (l *Leaf) Local(repo, tag string) string {
	return "localhost:" + installspec.RegistryPort + "/" + repo + ":" + tag
}

// Record writes the row of a version: one per repo and manifest digest, made
// on first sight. A second Record of the same version (a re-import that added
// a platform) keeps who, when, source and the first tag, and updates the
// platforms and size. An empty OrgID in img is stackr's own image (nil).
func (l *Leaf) Record(ctx context.Context, img store.RegistryImage) error {
	if img.Repo == "" || img.Digest == "" {
		return errs.Invalidf("image", "a version needs a repo and a digest")
	}
	have, err := l.images.GetByRepoDigest(ctx, img.Repo, img.Digest)
	switch {
	case err == nil:
		have.Platforms, have.Size = img.Platforms, img.Size
		return l.images.Update(ctx, have)
	case !errors.Is(err, errs.ErrNotFound):
		return err
	}
	if img.ID == "" {
		img.ID = uuid.NewString()
	}
	if img.CreatedAt.IsZero() {
		img.CreatedAt = time.Now().UTC()
	}
	if img.OrgID != nil && *img.OrgID == "" {
		img.OrgID = nil
	}
	return l.images.Create(ctx, img)
}

// JoinPlatforms is the platforms column: sorted, no repeats, comma separated.
func JoinPlatforms(p []string) string {
	p = slices.Clone(p)
	slices.Sort(p)
	return strings.Join(slices.Compact(p), ",")
}

// SplitPlatforms is JoinPlatforms back.
func SplitPlatforms(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
