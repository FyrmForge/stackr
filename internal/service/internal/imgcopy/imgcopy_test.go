package imgcopy

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// rig is one registry with a request log. looseIndex stores index manifests
// as they come, the way the cluster registry runs with index validation off;
// go-containerregistry's in-memory registry rejects an index whose children
// are absent, so the test fronts it with this.
type rig struct {
	host string
	mu   sync.Mutex
	reqs []string
	idx  map[string][]byte // "repo@sha256:.." or "repo:tag" -> index bytes
	// noReferrers answers the referrers API 404, as registry:3 does.
	noReferrers bool
}

func newRig(t *testing.T, loose bool, user, pass string) *rig {
	t.Helper()
	r := &rig{idx: map[string][]byte{}}
	inner := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if user != "" {
			if u, p, ok := req.BasicAuth(); !ok || u != user || p != pass {
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		r.mu.Lock()
		r.reqs = append(r.reqs, req.Method+" "+req.URL.Path)
		r.mu.Unlock()
		if r.noReferrers && strings.Contains(req.URL.Path, "/referrers/") {
			http.NotFound(w, req)
			return
		}
		if loose && r.serveIndex(w, req) {
			return
		}
		inner.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	r.host = strings.TrimPrefix(srv.URL, "http://")
	return r
}

func (r *rig) serveIndex(w http.ResponseWriter, req *http.Request) bool {
	i := strings.Index(req.URL.Path, "/manifests/")
	if i < 0 {
		return false
	}
	repo, ref := strings.TrimPrefix(req.URL.Path[:i], "/v2/"), req.URL.Path[i+len("/manifests/"):]
	r.mu.Lock()
	defer r.mu.Unlock()
	switch req.Method {
	case http.MethodPut:
		if !strings.Contains(req.Header.Get("Content-Type"), "index") && !strings.Contains(req.Header.Get("Content-Type"), "manifest.list") {
			return false
		}
		b, _ := io.ReadAll(req.Body)
		h, _, _ := v1.SHA256(strings.NewReader(string(b)))
		r.idx[repo+"@"+h.String()] = b
		r.idx[repo+":"+ref] = b
		w.Header().Set("Docker-Content-Digest", h.String())
		w.WriteHeader(http.StatusCreated)
		return true
	case http.MethodGet, http.MethodHead:
		b, ok := r.idx[repo+"@"+ref]
		if !ok {
			b, ok = r.idx[repo+":"+ref]
		}
		if !ok {
			return false
		}
		h, _, _ := v1.SHA256(strings.NewReader(string(b)))
		w.Header().Set("Docker-Content-Digest", h.String())
		w.Header().Set("Content-Type", string(types.OCIImageIndex))
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		if req.Method == http.MethodGet {
			_, _ = w.Write(b)
		}
		return true
	}
	return false
}

func (r *rig) mark() { r.mu.Lock(); r.reqs = nil; r.mu.Unlock() }

func (r *rig) count(prefix string) (n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, q := range r.reqs {
		if strings.HasPrefix(q, prefix) {
			n++
		}
	}
	return n
}

func archImage(t *testing.T, arch string) v1.Image {
	t.Helper()
	img, err := random.Image(512, 2)
	noErr(t, err)
	cf, err := img.ConfigFile()
	noErr(t, err)
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", arch
	img, err = mutate.ConfigFile(img, cf)
	noErr(t, err)
	return img
}

func pushIndex(t *testing.T, r *rig, repoTag string, arches ...string) (v1.ImageIndex, map[string]v1.Image) {
	t.Helper()
	idx := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	imgs := map[string]v1.Image{}
	for _, a := range arches {
		img := archImage(t, a)
		imgs[a] = img
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: a}},
		})
	}
	noErr(t, remote.WriteIndex(ref(t, r.host+"/"+repoTag), idx))
	return idx, imgs
}

func digestOf(t *testing.T, s string) string {
	t.Helper()
	d, err := remote.Head(ref(t, s))
	noErr(t, err)
	return d.Digest.String()
}

func TestCopyIndexFiltersAndKeepsDigest(t *testing.T) {
	src, dst := newRig(t, false, "", ""), newRig(t, true, "", "")
	idx, imgs := pushIndex(t, src, "lib/app:1", "amd64", "arm64")
	want, err := idx.Digest()
	noErr(t, err)
	s, d := src.host+"/lib/app:1", dst.host+"/org/lib/app:1"

	res, err := Copy(context.Background(), s, d, nil, nil, []string{"amd64"})
	noErr(t, err)
	eq(t, want.String(), res.Digest)
	eq(t, []string{"amd64"}, res.Platforms)
	positive(t, res.Size)
	eq(t, want.String(), digestOf(t, d))
	// The version is tagged by its digest too: a moved tag leaves it tagged.
	eq(t, want.String(), digestOf(t, dst.host+"/org/lib/app:"+strings.Replace(want.String(), ":", "-", 1)))

	amd, _ := imgs["amd64"].Digest()
	arm, _ := imgs["arm64"].Digest()
	_, err = remote.Head(ref(t, dst.host+"/org/lib/app@"+amd.String()))
	noErr(t, err)
	_, err = remote.Head(ref(t, dst.host+"/org/lib/app@"+arm.String()))
	isErr(t, err, "arm64 manifest must be absent")

	// A second run adds arm64 and copies only its blobs.
	dst.mark()
	src.mark()
	res, err = Copy(context.Background(), s, d, nil, nil, []string{"amd64", "arm64"})
	noErr(t, err)
	eq(t, want.String(), res.Digest)
	eq(t, []string{"amd64", "arm64"}, res.Platforms)
	armCfg, _ := imgs["arm64"].ConfigName()
	amdCfg, _ := imgs["amd64"].ConfigName()
	positive(t, src.count("GET /v2/lib/app/blobs/"+armCfg.String()))
	zero(t, src.count("GET /v2/lib/app/blobs/"+amdCfg.String()), "amd64 blobs were already there")
	_, err = remote.Head(ref(t, dst.host+"/org/lib/app@"+arm.String()))
	noErr(t, err)
}

func TestCopyHeadSkip(t *testing.T) {
	src, dst := newRig(t, false, "", ""), newRig(t, true, "", "")
	pushIndex(t, src, "lib/app:1", "amd64", "arm64")
	s, d := src.host+"/lib/app:1", dst.host+"/org/lib/app:1"
	_, err := Copy(context.Background(), s, d, nil, nil, []string{"amd64"})
	noErr(t, err)

	src.mark()
	res, err := Copy(context.Background(), s, d, nil, nil, []string{"amd64"})
	noErr(t, err)
	eq(t, []string{"amd64"}, res.Platforms)
	eq(t, 1, src.count("HEAD /v2/lib/app/manifests/"))
	zero(t, src.count("GET /v2/lib/"), "a held digest fetches nothing from src")
}

func TestCopySingleImage(t *testing.T) {
	src, dst := newRig(t, false, "", ""), newRig(t, true, "", "")
	img := archImage(t, "amd64")
	noErr(t, remote.Write(ref(t, src.host+"/lib/one:1"), img))
	want, _ := img.Digest()

	res, err := Copy(context.Background(), src.host+"/lib/one:1", dst.host+"/o/one:1", nil, nil, []string{"amd64"})
	noErr(t, err)
	eq(t, want.String(), res.Digest)
	eq(t, []string{"amd64"}, res.Platforms)
	eq(t, want.String(), digestOf(t, dst.host+"/o/one:1"))

	_, err = Copy(context.Background(), src.host+"/lib/one:1", dst.host+"/o/two:1", nil, nil, []string{"arm64"})
	errHas(t, err, "linux/amd64")
	_, err = remote.Head(ref(t, dst.host+"/o/two:1"))
	isErr(t, err, "nothing half-copied")
}

func TestCopyMissingPlatform(t *testing.T) {
	src, dst := newRig(t, false, "", ""), newRig(t, true, "", "")
	pushIndex(t, src, "lib/app:1", "amd64")
	_, err := Copy(context.Background(), src.host+"/lib/app:1", dst.host+"/o/app:1", nil, nil, []string{"amd64", "arm64"})
	errHas(t, err, "linux/arm64")
	errHas(t, err, "linux/amd64")
	zero(t, dst.count("PUT "), "nothing copied when a wanted platform is missing")
}

func TestCopyPrivateSource(t *testing.T) {
	src, dst := newRig(t, false, "bot", "s3cret-fake"), newRig(t, true, "", "")
	good := &authn.Basic{Username: "bot", Password: "s3cret-fake"}
	noErr(t, remote.Write(ref(t, src.host+"/p/app:1"), archImage(t, "amd64"), remote.WithAuth(good)))

	// refused either way: one DeniedError naming the ref, no registry URL
	for _, a := range []authn.Authenticator{&authn.Basic{Username: "bot", Password: "wrong"}, nil} {
		_, err := Copy(context.Background(), src.host+"/p/app:1", dst.host+"/o/app:1", a, nil, []string{"amd64"})
		var no *DeniedError
		eq(t, true, errors.As(err, &no), err)
		eq(t, src.host+"/p/app:1 is private or missing", err.Error())
	}

	res, err := Copy(context.Background(), src.host+"/p/app:1", dst.host+"/o/app:1", good, nil, []string{"amd64"})
	noErr(t, err)
	eq(t, []string{"amd64"}, res.Platforms)
}

func ref(t *testing.T, s string) name.Reference {
	t.Helper()
	r, err := name.ParseReference(s)
	noErr(t, err)
	return r
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func isErr(t *testing.T, err error, msg ...any) {
	t.Helper()
	if err == nil {
		t.Fatal(append([]any{"expected an error"}, msg...)...)
	}
}

func errHas(t *testing.T, err error, s string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), s) {
		t.Fatalf("error %v does not mention %q", err, s)
	}
}

func eq(t *testing.T, want, got any, msg ...any) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Errorf("want %v, got %v %v", want, got, msg)
	}
}

func positive[T int | int64](t *testing.T, n T) {
	t.Helper()
	if n <= 0 {
		t.Errorf("want > 0, got %d", n)
	}
}

func zero(t *testing.T, n int, msg ...any) {
	t.Helper()
	if n != 0 {
		t.Errorf("want 0, got %d %v", n, msg)
	}
}

// An index with BuildKit attestations (Docker Hub's official images): the
// wanted platform's attestation is copied with it, the others' are not, and
// a copy that lacks it is not taken as held.
func TestCopyKeepsAttestations(t *testing.T) {
	src, dst := newRig(t, false, "", ""), newRig(t, true, "", "")
	idx := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	att := map[string]v1.Hash{}
	for _, a := range []string{"amd64", "arm64"} {
		img := archImage(t, a)
		h, _ := img.Digest()
		at, err := random.Image(64, 1)
		noErr(t, err)
		att[a], _ = at.Digest()
		idx = mutate.AppendManifests(idx,
			mutate.IndexAddendum{Add: img, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: a}}},
			mutate.IndexAddendum{Add: at, Descriptor: v1.Descriptor{
				Platform:    &v1.Platform{OS: "unknown", Architecture: "unknown"},
				Annotations: map[string]string{"vnd.docker.reference.type": "attestation-manifest", "vnd.docker.reference.digest": h.String()},
			}})
	}
	noErr(t, remote.WriteIndex(ref(t, src.host+"/lib/app:1"), idx))
	s, d := src.host+"/lib/app:1", dst.host+"/org/lib/app:1"

	res, err := Copy(context.Background(), s, d, nil, nil, []string{"amd64"})
	noErr(t, err)
	eq(t, []string{"amd64"}, res.Platforms)
	amdAtt := ref(t, dst.host+"/org/lib/app@"+att["amd64"].String())
	_, err = remote.Head(amdAtt)
	noErr(t, err)
	_, err = remote.Head(ref(t, dst.host+"/org/lib/app@"+att["arm64"].String()))
	isErr(t, err, "arm64 attestation must be absent")

	// An earlier copy without the attestation is copied again.
	noErr(t, remote.Delete(amdAtt))
	_, err = Copy(context.Background(), s, d, nil, nil, []string{"amd64"})
	noErr(t, err)
	_, err = remote.Head(amdAtt)
	noErr(t, err)
}

// A digest pinned src that dst holds with every wanted platform is not
// contacted at all; with a platform missing it is fetched.
func TestCopyPinnedHeldAsksNobody(t *testing.T) {
	src, dst := newRig(t, false, "", ""), newRig(t, true, "", "")
	pushIndex(t, src, "lib/app:1", "amd64", "arm64")
	pin := src.host + "/lib/app@" + digestOf(t, src.host+"/lib/app:1")
	d := dst.host + "/org/lib/app:sha256-x"
	_, err := Copy(context.Background(), pin, d, nil, nil, []string{"amd64"})
	noErr(t, err)

	src.mark()
	res, err := Copy(context.Background(), pin, d, nil, nil, []string{"amd64"})
	noErr(t, err)
	eq(t, []string{"amd64"}, res.Platforms)
	zero(t, src.count(""), "a held pinned digest sends no request to src")

	res, err = Copy(context.Background(), pin, d, nil, nil, []string{"amd64", "arm64"})
	noErr(t, err)
	eq(t, []string{"amd64", "arm64"}, res.Platforms)
	positive(t, src.count("GET /v2/lib/app/manifests/"))
}

// Only an unreachable src (down, 5xx, 429) is an UnreachableError; a 401 or
// 404 is an answer. Held reads dst alone.
func TestCopyUnreachable(t *testing.T) {
	dst := newRig(t, true, "", "")
	d := dst.host + "/org/app:1"
	for code, want := range map[int]bool{503: true, 429: true, 401: false, 404: false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		_, err := Copy(context.Background(), strings.TrimPrefix(srv.URL, "http://")+"/lib/app:1", d, nil, nil, []string{"amd64"})
		srv.Close()
		var ue *UnreachableError
		eq(t, want, errors.As(err, &ue), code)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	gone := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	_, err := Copy(context.Background(), gone+"/lib/app:1", d, nil, nil, []string{"amd64"})
	var ue *UnreachableError
	eq(t, true, errors.As(err, &ue), "closed server")

	_, ok, err := Held(context.Background(), d, nil, []string{"amd64"})
	noErr(t, err)
	eq(t, false, ok)
}

// An attestation naming its image as an OCI subject leaves no
// "sha256-<child>" referrers tag behind on a registry without the referrers
// API: the only tags are the import's own.
func TestCopyNoReferrersTag(t *testing.T) {
	src, dst := newRig(t, false, "", ""), newRig(t, true, "", "")
	dst.noReferrers = true
	img := archImage(t, "amd64")
	h, _ := img.Digest()
	sz, _ := img.Size()
	mt, _ := img.MediaType()
	at, err := random.Image(64, 1)
	noErr(t, err)
	at = mutate.Subject(at, v1.Descriptor{MediaType: mt, Digest: h, Size: sz}).(v1.Image)
	idx := mutate.AppendManifests(mutate.IndexMediaType(empty.Index, types.OCIImageIndex),
		mutate.IndexAddendum{Add: img, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: at, Descriptor: v1.Descriptor{
			Platform:    &v1.Platform{OS: "unknown", Architecture: "unknown"},
			Annotations: map[string]string{"vnd.docker.reference.type": "attestation-manifest", "vnd.docker.reference.digest": h.String()},
		}})
	noErr(t, remote.WriteIndex(ref(t, src.host+"/lib/app:1"), idx))
	_, err = Copy(context.Background(), src.host+"/lib/app:1", dst.host+"/org/lib/app:1", nil, nil, []string{"amd64"})
	noErr(t, err)
	_, err = remote.Head(ref(t, dst.host+"/org/lib/app@"+h.String()))
	noErr(t, err)
	_, err = remote.Head(ref(t, dst.host+"/org/lib/app:"+strings.Replace(h.String(), ":", "-", 1)))
	isErr(t, err, "the image's digest tag must be absent")
}
