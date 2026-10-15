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
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/imgcopy"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
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
	// base is the registry's HTTP address; tr reaches it (the registry's
	// unix socket), nil = TCP to base (tests).
	base string
	tr   http.RoundTripper
	// copy is import.go's filtered copy (imgcopy.Copy).
	copy copier
	// gate is shut while the cleanup sweep runs (clean.go); Import waits on it.
	gate regfront.Gate
}

// New is the leaf over the registry serving socket (installspec.RegistrySocket).
// Requests name localhost:5000, so the registry's upload URLs do too; the
// transport dials the socket whatever the address.
func New(images store.RegistryImageStore, socket string) *Leaf {
	l := &Leaf{images: images, base: "http://localhost:" + installspec.RegistryPort, copy: imgcopy.Copy}
	if socket != "" {
		l.tr = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}
	}
	return l
}

// WithBase points the leaf at another registry address over TCP (tests).
func (l *Leaf) WithBase(base string) *Leaf {
	l.base, l.tr = base, nil
	return l
}

// Transport reaches the registry: the registry front's backend, and the
// controller's own writes. nil = TCP.
func (l *Leaf) Transport() http.RoundTripper { return l.tr }

// Backend is the registry's address and transport, for the registry fronts.
func (l *Leaf) Backend() (*url.URL, http.RoundTripper) {
	u, _ := url.Parse(l.base) // set by New or WithBase: a URL
	return u, l.tr
}

// remote is go-containerregistry's options for the registry.
func (l *Leaf) remote(ctx context.Context) []remote.Option {
	o := []remote.Option{remote.WithContext(ctx)}
	if l.tr != nil {
		o = append(o, remote.WithTransport(l.tr))
	}
	return o
}

// Local is the name a repo and tag has on every node once loaded (Image);
// nothing listens at localhost:5000. Nothing outside this leaf builds one by
// hand.
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
