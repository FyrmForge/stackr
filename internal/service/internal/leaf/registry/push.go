package registry

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Push is a manifest push that has just succeeded at the registry front.
type Push struct {
	// OrgID is the pushing repo's org, "" for stackr's own (stackr.system).
	OrgID string
	Repo  string
	// Reference is what the manifest was put under: a tag, or a digest.
	Reference string
	// Digest is the registry's digest for it; "" = read it from the registry.
	Digest string
	// By is the user id for a developer push, the node id for a build.
	By string
}

// Pushed records a finished push (a build's or a developer's): it reads the
// manifest back from the registry for its platforms and size and Records the
// version as built. Called from regfront's Pushed callback and after a build's
// own push, so it must be quick and its errors are only logged: a push is
// never failed for the record's sake.
//
// A push by digest is not recorded: buildx puts each platform manifest and
// attestation up by digest before the index goes up under the tag, and a row
// for a child would let the cleanup treat it as a version of its own and
// delete it from under the index.
func (l *Leaf) Pushed(ctx context.Context, p Push) error {
	if strings.HasPrefix(p.Reference, "sha256:") || p.Reference == "" {
		return nil
	}
	ref, err := name.ParseReference(strings.TrimPrefix(l.base, "http://")+"/"+p.Repo+":"+p.Reference, name.Insecure)
	if err != nil {
		return err
	}
	desc, err := remote.Get(ref, remote.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("read %s:%s back: %w", p.Repo, p.Reference, err)
	}
	platforms, size, err := measure(desc)
	if err != nil {
		return fmt.Errorf("read %s:%s back: %w", p.Repo, p.Reference, err)
	}
	if p.Digest == "" {
		p.Digest = desc.Digest.String()
	}
	img := store.RegistryImage{
		Repo: p.Repo, Tag: p.Reference, Digest: p.Digest, Platforms: JoinPlatforms(platforms),
		Source: SourceBuilt, Size: size, CreatedBy: p.By,
	}
	if p.OrgID != "" {
		img.OrgID = &p.OrgID
	} else {
		img.Source = SourceSystem
	}
	return l.Record(ctx, img)
}

// measure is the CPU types and the bytes behind a manifest: for an index the
// children that are stored (a child it names but the registry lacks is not
// counted), for an image its own.
func measure(desc *remote.Descriptor) (platforms []string, size int64, err error) {
	if !desc.MediaType.IsIndex() {
		img, err := desc.Image()
		if err != nil {
			return nil, 0, err
		}
		arch, n, err := imageInfo(img)
		return []string{arch}, n, err
	}
	idx, err := desc.ImageIndex()
	if err != nil {
		return nil, 0, err
	}
	m, err := idx.IndexManifest()
	if err != nil {
		return nil, 0, err
	}
	for _, c := range m.Manifests {
		if c.Platform == nil || c.Platform.Architecture == "" || c.Platform.Architecture == "unknown" {
			continue // attestations and the like
		}
		img, err := idx.Image(c.Digest)
		if err != nil {
			continue // named by the index, not stored here
		}
		_, n, err := imageInfo(img)
		if err != nil {
			continue
		}
		platforms, size = append(platforms, c.Platform.Architecture), size+n
	}
	return platforms, size, nil
}

func imageInfo(img v1.Image) (arch string, size int64, err error) {
	cfg, err := img.ConfigFile()
	if err != nil {
		return "", 0, err
	}
	m, err := img.Manifest()
	if err != nil {
		return "", 0, err
	}
	raw, err := img.RawManifest()
	if err != nil {
		return "", 0, err
	}
	size = int64(len(raw)) + m.Config.Size
	for _, l := range m.Layers {
		size += l.Size
	}
	return cfg.Architecture, size, nil
}
