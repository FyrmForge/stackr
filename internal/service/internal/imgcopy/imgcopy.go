// Package imgcopy copies an image from an outside registry into the cluster
// registry, keeping only the CPU types the cluster has. It is a thin layer on
// go-containerregistry's remote package; crane's own filtered copy drops the
// index and so changes the digest, which this must not.
package imgcopy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
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
//   - A src pinned by digest that dst already holds with every wanted platform
//     is not contacted at all: the content behind a digest never changes.
//   - Credentials go only to the registry they were given for (srcAuth to
//     src's, dstAuth to dst's) and are never logged.
func Copy(ctx context.Context, src, dst string, srcAuth, dstAuth authn.Authenticator, platforms []string) (Result, error) {
	srcRef, err := name.ParseReference(src)
	if err != nil {
		return Result{}, fmt.Errorf("source %q: %w", src, err)
	}
	dstRef, err := name.ParseReference(dst)
	if err != nil {
		return Result{}, fmt.Errorf("destination %q: %w", dst, err)
	}
	if _, ok := dstRef.(name.Tag); !ok {
		return Result{}, fmt.Errorf("destination %q needs a tag", dst)
	}
	if len(platforms) == 0 {
		return Result{}, errors.New("no platforms wanted")
	}
	wanted := make([]want, 0, len(platforms))
	for _, p := range platforms {
		wanted = append(wanted, parseWant(p))
	}
	so := []remote.Option{remote.WithContext(ctx), remote.WithAuth(orAnon(srcAuth))}
	do := []remote.Option{remote.WithContext(ctx), remote.WithAuth(orAnon(dstAuth))}

	// A digest pin names fixed content: if dst holds it, src is not asked.
	if pin, ok := srcRef.(name.Digest); ok {
		if res, ok, err := heldAt(dstRef, pin.DigestStr(), wanted, do); err != nil || ok {
			return res, err
		}
	} else if h, err := remote.Head(srcRef, so...); err == nil {
		// HEAD first. A registry that refuses HEAD falls through to the GET.
		if res, ok, err := heldAt(dstRef, h.Digest.String(), wanted, do); err != nil || ok {
			return res, err
		}
	}

	d, err := remote.Get(srcRef, so...)
	if err != nil {
		if unreachable(err) {
			return Result{}, &UnreachableError{Registry: srcRef.Context().RegistryStr(), Err: fmt.Errorf("source %s: %w", srcRef, err)}
		}
		return Result{}, fmt.Errorf("source %s: %w", srcRef, err)
	}
	if d.MediaType.IsIndex() {
		err = copyIndex(d, srcRef, dstRef, wanted, do)
	} else {
		err = copyImage(d, srcRef, dstRef, wanted, do)
	}
	if err != nil {
		return Result{}, err
	}
	// The original bytes under the tag: the digest stays upstream's.
	if err := remote.Put(dstRef, d, do...); err != nil {
		return Result{}, fmt.Errorf("tag %s: %w", dstRef, err)
	}
	dd, err := remote.Get(dstRef.Context().Digest(d.Digest.String()), do...)
	if err != nil {
		return Result{}, err
	}
	res, _, err := held(dstRef, dd, wanted, do)
	return res, err
}

// heldAt is held for the manifest dst stores under digest; ok is false when
// dst has none or lacks a wanted platform.
func heldAt(dstRef name.Reference, digest string, wanted []want, do []remote.Option) (Result, bool, error) {
	d, err := remote.Get(dstRef.Context().Digest(digest), do...)
	switch {
	case err == nil:
		return held(dstRef, d, wanted, do)
	case notFound(err):
		return Result{}, false, nil
	}
	return Result{}, false, fmt.Errorf("destination %s: %w", dstRef.Context(), err)
}

// Held reports what dst (a ref with a tag) stores behind that tag, and
// whether it covers every platform, without contacting any other registry.
func Held(ctx context.Context, dst string, dstAuth authn.Authenticator, platforms []string) (Result, bool, error) {
	dstRef, err := name.ParseReference(dst)
	if err != nil {
		return Result{}, false, fmt.Errorf("destination %q: %w", dst, err)
	}
	wanted := make([]want, 0, len(platforms))
	for _, p := range platforms {
		wanted = append(wanted, parseWant(p))
	}
	do := []remote.Option{remote.WithContext(ctx), remote.WithAuth(orAnon(dstAuth))}
	h, err := remote.Head(dstRef, do...)
	if notFound(err) {
		return Result{}, false, nil
	} else if err != nil {
		return Result{}, false, fmt.Errorf("destination %s: %w", dstRef.Context(), err)
	}
	return heldAt(dstRef, h.Digest.String(), wanted, do)
}

// UnreachableError is Copy's answer when src could not be asked: a network
// error, timeout or DNS failure, or a 5xx or 429 from its registry. A 401,
// 403 or 404 is an answer, not this: a revoked credential or a deleted image
// must fail, not fall back to an older copy.
type UnreachableError struct {
	Registry string
	Err      error
}

func (e *UnreachableError) Error() string { return e.Err.Error() }
func (e *UnreachableError) Unwrap() error { return e.Err }

func unreachable(err error) bool {
	var te *transport.Error
	if errors.As(err, &te) {
		return te.StatusCode == http.StatusTooManyRequests || te.StatusCode >= 500
	}
	var ne net.Error
	return errors.As(err, &ne)
}

func copyIndex(d *remote.Descriptor, srcRef, dstRef name.Reference, wanted []want, do []remote.Option) error {
	idx, err := d.ImageIndex()
	if err != nil {
		return err
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return err
	}
	// Resolve every wanted platform before copying anything.
	var picks []v1.Hash
	for _, w := range wanted {
		var found *v1.Descriptor
		for i := range im.Manifests {
			if w.matches(im.Manifests[i].Platform) {
				found = &im.Manifests[i]
				break
			}
		}
		if found == nil {
			return fmt.Errorf("%s has no %s image, it has: %s", srcRef, w, strings.Join(platformsOf(im), ", "))
		}
		picks = append(picks, found.Digest)
	}
	for _, h := range picks {
		img, err := idx.Image(h)
		if err != nil {
			return err
		}
		// Write skips blobs the destination already has.
		if err := remote.Write(dstRef.Context().Digest(h.String()), img, do...); err != nil {
			return fmt.Errorf("copy %s: %w", h, err)
		}
	}
	return nil
}

func copyImage(d *remote.Descriptor, srcRef, dstRef name.Reference, wanted []want, do []remote.Option) error {
	img, err := d.Image()
	if err != nil {
		return err
	}
	p, err := imagePlatform(img)
	if err != nil {
		return err
	}
	if !matchesAny(wanted, p) {
		return fmt.Errorf("%s is a %s/%s image, wanted: %s", srcRef, p.OS, p.Architecture, wantList(wanted))
	}
	if err := remote.Write(dstRef.Context().Digest(d.Digest.String()), img, do...); err != nil {
		return fmt.Errorf("copy %s: %w", d.Digest, err)
	}
	return nil
}

// held reports what dst stores behind d and whether that covers every wanted
// platform. When it does, the tag is made to point at d too.
func held(dstRef name.Reference, d *remote.Descriptor, wanted []want, do []remote.Option) (Result, bool, error) {
	res := Result{Digest: d.Digest.String()}
	have := map[string]*v1.Platform{}
	add := func(img v1.Image, p *v1.Platform) error {
		m, err := img.Manifest()
		if err != nil {
			return err
		}
		raw, err := img.RawManifest()
		if err != nil {
			return err
		}
		res.Size += int64(len(raw)) + m.Config.Size
		for _, l := range m.Layers {
			res.Size += l.Size
		}
		have[p.Architecture] = p
		return nil
	}
	if d.MediaType.IsIndex() {
		idx, err := d.ImageIndex()
		if err != nil {
			return res, false, err
		}
		im, err := idx.IndexManifest()
		if err != nil {
			return res, false, err
		}
		for _, c := range im.Manifests {
			if c.Platform == nil || c.Platform.OS != "linux" {
				continue
			}
			cd, err := remote.Get(dstRef.Context().Digest(c.Digest.String()), do...)
			if notFound(err) {
				continue
			} else if err != nil {
				return res, false, err
			}
			img, err := cd.Image()
			if err != nil {
				return res, false, err
			}
			if err := add(img, c.Platform); err != nil {
				return res, false, err
			}
		}
		for _, w := range wanted {
			ok := false
			for _, p := range have {
				ok = ok || w.matches(p)
			}
			if !ok {
				return res, false, nil
			}
		}
	} else {
		img, err := d.Image()
		if err != nil {
			return res, false, err
		}
		p, err := imagePlatform(img)
		if err != nil {
			return res, false, err
		}
		if !matchesAny(wanted, p) {
			return res, false, nil
		}
		if err := add(img, p); err != nil {
			return res, false, err
		}
	}
	for a := range have {
		res.Platforms = append(res.Platforms, a)
	}
	sort.Strings(res.Platforms)
	if h, err := remote.Head(dstRef, do...); err != nil || h.Digest != d.Digest {
		if err := remote.Put(dstRef, d, do...); err != nil {
			return res, false, fmt.Errorf("tag %s: %w", dstRef, err)
		}
	}
	return res, true, nil
}

func imagePlatform(img v1.Image) (*v1.Platform, error) {
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	return &v1.Platform{OS: cf.OS, Architecture: cf.Architecture, Variant: cf.Variant}, nil
}

// want is one wanted platform. ponytail: results name the CPU type only, so
// two variants of one arch (arm/v6, arm/v7) collapse into one name.
type want struct{ os, arch, variant string }

func parseWant(p string) want {
	parts := strings.Split(p, "/")
	if len(parts) == 1 {
		return want{"linux", parts[0], ""}
	}
	w := want{parts[0], parts[1], ""}
	if len(parts) > 2 {
		w.variant = parts[2]
	}
	return w
}

func (w want) String() string {
	if w.variant != "" {
		return w.os + "/" + w.arch + "/" + w.variant
	}
	return w.os + "/" + w.arch
}

func (w want) matches(p *v1.Platform) bool {
	return p != nil && p.OS == w.os && p.Architecture == w.arch && (w.variant == "" || p.Variant == w.variant)
}

func matchesAny(ws []want, p *v1.Platform) bool {
	for _, w := range ws {
		if w.matches(p) {
			return true
		}
	}
	return false
}

func wantList(ws []want) string {
	s := make([]string, len(ws))
	for i, w := range ws {
		s[i] = w.String()
	}
	return strings.Join(s, ", ")
}

func platformsOf(im *v1.IndexManifest) []string {
	var out []string
	for _, c := range im.Manifests {
		if c.Platform != nil && c.Platform.OS != "unknown" {
			out = append(out, c.Platform.OS+"/"+c.Platform.Architecture)
		}
	}
	return out
}

func orAnon(a authn.Authenticator) authn.Authenticator {
	if a == nil {
		return authn.Anonymous
	}
	return a
}

func notFound(err error) bool {
	var te *transport.Error
	return errors.As(err, &te) && te.StatusCode == http.StatusNotFound
}
