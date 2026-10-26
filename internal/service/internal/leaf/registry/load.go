package registry

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// Image is what repo at (a tag, or a "sha256:..." digest) is for a node of
// CPU type arch ("amd64"): the one platform's image, read over the
// registry's socket, as a tarball docker load takes. An index gives its child
// for arch, never the index itself (an import keeps upstream's index bytes,
// whose other children are not stored). Nothing is read until Write.
func (l *Leaf) Image(ctx context.Context, repo, at, arch string) (docker.ClusterImage, error) {
	base := strings.TrimPrefix(l.base, "http://") + "/" + repo
	var ref name.Reference
	var err error
	if strings.HasPrefix(at, "sha256:") {
		ref, err = name.NewDigest(base+"@"+at, name.Insecure)
	} else {
		ref, err = name.NewTag(base+":"+at, name.Insecure)
	}
	if err != nil {
		return docker.ClusterImage{}, err
	}
	d, err := remote.Get(ref, l.remote(ctx)...)
	if notFound(err) {
		return docker.ClusterImage{}, fmt.Errorf("%s:%s: %w", repo, at, docker.ErrNoClusterImage)
	} else if err != nil {
		return docker.ClusterImage{}, fmt.Errorf("%s:%s: %w", repo, at, err)
	}
	var img v1.Image
	if d.MediaType.IsIndex() {
		idx, err := d.ImageIndex()
		if err != nil {
			return docker.ClusterImage{}, err
		}
		im, err := idx.IndexManifest()
		if err != nil {
			return docker.ClusterImage{}, err
		}
		for _, c := range im.Manifests {
			if p := c.Platform; p != nil && p.OS == "linux" && p.Architecture == arch {
				if img, err = idx.Image(c.Digest); err != nil {
					return docker.ClusterImage{}, err
				}
				break
			}
		}
		if img == nil {
			return docker.ClusterImage{}, fmt.Errorf("%s:%s has no linux/%s image", repo, at, arch)
		}
	} else if img, err = d.Image(); err != nil {
		return docker.ClusterImage{}, err
	}
	mdg, err := img.Digest()
	if err != nil {
		return docker.ClusterImage{}, err
	}
	cdg, err := img.ConfigName()
	if err != nil {
		return docker.ClusterImage{}, err
	}
	return docker.ClusterImage{
		Digest: d.Digest.String(),
		IDs:    []string{mdg.String(), cdg.String()},
		Write:  func(w io.Writer, tag string) error { return writeLoad(w, img, tag) },
	}, nil
}

// writeLoad writes img as docker save does: an OCI layout (index.json names
// the manifest, so the containerd image store keeps its digest as the image
// id) and the manifest.json the classic store reads (its id is the config's
// digest). Layers go as the registry stores them, compressed. tag is the
// name the loaded image gets.
func writeLoad(w io.Writer, img v1.Image, tag string) error {
	tw := tar.NewWriter(w)
	put := func(path string, size int64, r io.Reader) error {
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0o444, Size: size, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		n, err := io.Copy(tw, r)
		if err == nil && n != size {
			err = fmt.Errorf("%s: %d bytes, the manifest says %d", path, n, size)
		}
		return err
	}
	putBytes := func(path string, b []byte) error { return put(path, int64(len(b)), bytes.NewReader(b)) }
	blob := func(h v1.Hash) string { return "blobs/" + h.Algorithm + "/" + h.Hex }

	m, err := img.Manifest()
	if err != nil {
		return err
	}
	raw, err := img.RawManifest()
	if err != nil {
		return err
	}
	mdg, err := img.Digest()
	if err != nil {
		return err
	}
	cfg, err := img.RawConfigFile()
	if err != nil {
		return err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return err
	}
	if err := putBytes("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return err
	}
	if err := putBytes(blob(m.Config.Digest), cfg); err != nil {
		return err
	}
	var layers []string
	for _, ld := range m.Layers {
		l, err := img.LayerByDigest(ld.Digest)
		if err != nil {
			return err
		}
		rc, err := l.Compressed()
		if err != nil {
			return err
		}
		err = put(blob(ld.Digest), ld.Size, rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
		layers = append(layers, blob(ld.Digest))
	}
	if err := putBytes(blob(mdg), raw); err != nil {
		return err
	}
	mt, _ := img.MediaType()
	_, short, _ := strings.Cut(tag[strings.LastIndex(tag, "/")+1:], ":")
	index, err := json.Marshal(v1.IndexManifest{
		SchemaVersion: 2, MediaType: "application/vnd.oci.image.index.v1+json",
		Manifests: []v1.Descriptor{{
			MediaType: mt, Digest: mdg, Size: int64(len(raw)),
			Platform:    &v1.Platform{OS: cf.OS, Architecture: cf.Architecture, Variant: cf.Variant},
			Annotations: map[string]string{"io.containerd.image.name": tag, "org.opencontainers.image.ref.name": short},
		}},
	})
	if err != nil {
		return err
	}
	if err := putBytes("index.json", index); err != nil {
		return err
	}
	legacy, err := json.Marshal([]struct {
		Config   string
		RepoTags []string
		Layers   []string
	}{{blob(m.Config.Digest), []string{tag}, layers}})
	if err != nil {
		return err
	}
	if err := putBytes("manifest.json", legacy); err != nil {
		return err
	}
	return tw.Close()
}

func notFound(err error) bool {
	var te *transport.Error
	return errors.As(err, &te) && te.StatusCode == http.StatusNotFound
}
