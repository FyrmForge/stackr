package registry_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcr "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/FyrmForge/stackr/internal/service/internal/imgcopy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Import copies under the org's import name, with the credential going to the
// copy as the source's, and records an imported version; a repeat with a new
// platform updates the row instead of adding one.
func TestImport(t *testing.T) {
	st := servicetest.Store(t)
	org := seedOrg(t, st)
	var gotSrc, gotDst string
	var gotAuth authn.Authenticator
	l := registry.New(st.RegistryImages, "").WithCopier(func(_ context.Context, src, dst string, srcAuth authn.Authenticator, _ http.RoundTripper, p []string) (imgcopy.Result, error) {
		gotSrc, gotDst, gotAuth = src, dst, srcAuth
		return imgcopy.Result{Digest: "sha256:idx", Platforms: p, Size: 10 * int64(len(p))}, nil
	})

	auth := authn.FromConfig(authn.AuthConfig{Username: "u", Password: "p"})
	got, err := l.Import(ctx, registry.ImportRequest{
		OrgID: org, OrgSlug: "acme", Ref: "postgres:16", Auth: auth, Platforms: []string{"amd64"}, By: "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.LocalRef != "localhost:5000/acme/docker.io/library/postgres:16" || got.Digest != "sha256:idx" {
		t.Fatalf("imported = %+v", got)
	}
	if gotSrc != "postgres:16" || gotAuth != auth || !strings.HasSuffix(gotDst, "/acme/docker.io/library/postgres:16") {
		t.Fatalf("copy(%q -> %q, auth %v)", gotSrc, gotDst, gotAuth)
	}
	row, err := st.RegistryImages.GetByRepoDigest(ctx, "acme/docker.io/library/postgres", "sha256:idx")
	if err != nil {
		t.Fatal(err)
	}
	if row.Source != "imported" || row.UpstreamRef != "postgres:16" || row.Platforms != "amd64" || row.Size != 10 ||
		row.CreatedBy != "u1" || row.OrgID == nil || *row.OrgID != org || row.Tag != "16" {
		t.Fatalf("row = %+v", row)
	}

	if _, err := l.Import(ctx, registry.ImportRequest{OrgID: org, OrgSlug: "acme", Ref: "postgres:16", Platforms: []string{"amd64", "arm64"}}); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.RegistryImages.ListByRepo(ctx, "acme/docker.io/library/postgres")
	if len(rows) != 1 || rows[0].Platforms != "amd64,arm64" || rows[0].CreatedBy != "u1" {
		t.Fatalf("rows after a new platform = %+v", rows)
	}
}

// A digest pinned ref lands under its sha256-<hex> tag; no platforms is an
// error; a failed copy records nothing.
func TestImportEdges(t *testing.T) {
	st := servicetest.Store(t)
	org := seedOrg(t, st)
	boom := errors.New("denied")
	var order []string
	l := registry.New(st.RegistryImages, "").
		WithCopier(func(_ context.Context, _, dst string, _ authn.Authenticator, _ http.RoundTripper, _ []string) (imgcopy.Result, error) {
			order = append(order, "copy "+dst[strings.Index(dst, "/")+1:])
			return imgcopy.Result{}, boom
		})
	req := registry.ImportRequest{OrgID: org, OrgSlug: "acme", Ref: "ghcr.io/foo/bar@sha256:abcd", Platforms: []string{"amd64"}}
	if _, err := l.Import(ctx, req); !errors.Is(err, boom) {
		t.Fatalf("copy error lost: %v", err)
	}
	if strings.Join(order, ";") != "copy acme/ghcr.io/foo/bar:sha256-abcd" {
		t.Fatalf("order = %v", order)
	}
	if rows, _ := st.RegistryImages.ListAll(ctx); len(rows) != 0 {
		t.Fatalf("a failed copy recorded %+v", rows)
	}
	req.Platforms = nil
	if _, err := l.Import(ctx, req); err == nil {
		t.Error("an import with no platforms ran")
	}
}

// An import during a gated sweep waits for the gate the cleanup closes, then
// proceeds; a context that ends first stops it.
func TestImportWaitsOnSweepGate(t *testing.T) {
	st := servicetest.Store(t)
	org := seedOrg(t, st)
	copied := make(chan struct{}, 1)
	l := registry.New(st.RegistryImages, "").
		WithCopier(func(context.Context, string, string, authn.Authenticator, http.RoundTripper, []string) (imgcopy.Result, error) {
			copied <- struct{}{}
			return imgcopy.Result{Digest: "sha256:abc", Platforms: []string{"amd64"}}, nil
		})
	req := registry.ImportRequest{OrgID: org, OrgSlug: "acme", Ref: "ghcr.io/foo/bar:1", Platforms: []string{"amd64"}}
	release := make(chan struct{})
	swept := make(chan struct{})
	go func() {
		_ = l.WhileGated(ctx, func(context.Context) error { close(swept); <-release; return nil })
	}()
	<-swept
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := l.Import(short, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("import during the sweep = %v, want a deadline", err)
	}
	done := make(chan error, 1)
	go func() { _, err := l.Import(ctx, req); done <- err }()
	select {
	case <-copied:
		t.Fatal("import copied while the gate was shut")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("import after the sweep: %v", err)
	}
}

// A sweep started while an import copies waits for the import to finish:
// the garbage collect would take the blobs it put before its tag goes up.
func TestSweepWaitsForImportInFlight(t *testing.T) {
	st := servicetest.Store(t)
	org := seedOrg(t, st)
	copying, finish := make(chan struct{}), make(chan struct{})
	l := registry.New(st.RegistryImages, "").
		WithCopier(func(context.Context, string, string, authn.Authenticator, http.RoundTripper, []string) (imgcopy.Result, error) {
			close(copying)
			<-finish
			return imgcopy.Result{Digest: "sha256:abc", Platforms: []string{"amd64"}}, nil
		})
	req := registry.ImportRequest{OrgID: org, OrgSlug: "acme", Ref: "ghcr.io/foo/bar:1", Platforms: []string{"amd64"}}
	imported := make(chan error, 1)
	go func() { _, err := l.Import(ctx, req); imported <- err }()
	<-copying
	swept := make(chan error, 1)
	go func() { swept <- l.WhileGated(ctx, func(context.Context) error { return nil }) }()
	select {
	case <-swept:
		t.Fatal("the sweep ran while an import was copying")
	case <-time.After(100 * time.Millisecond):
	}
	close(finish)
	if err := <-imported; err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := <-swept; err != nil {
		t.Fatalf("sweep: %v", err)
	}
}

func platformImage(t *testing.T, arch string) v1.Image {
	t.Helper()
	img, err := random.Image(64, 2)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := img.ConfigFile()
	cfg.Architecture, cfg.OS = arch, "linux"
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// Pushed reads the manifest back: an image gives its architecture and size,
// an index the platforms it holds (an absent child is not counted); a push by
// digest records nothing.
func TestPushed(t *testing.T) {
	reg := httptest.NewServer(ggcr.New())
	defer reg.Close()
	host := strings.TrimPrefix(reg.URL, "http://")
	st := servicetest.Store(t)
	org := seedOrg(t, st)
	l := registry.New(st.RegistryImages, "").WithBase(reg.URL)

	amd, arm := platformImage(t, "amd64"), platformImage(t, "arm64")
	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: amd, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: arm, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
	)
	write := func(ref string, put func(name.Reference) error) {
		t.Helper()
		r, err := name.ParseReference(host+"/"+ref, name.Insecure)
		if err != nil {
			t.Fatal(err)
		}
		if err := put(r); err != nil {
			t.Fatal(err)
		}
	}
	write("acme/shop/api:v1", func(r name.Reference) error { return remote.Write(r, amd) })
	write("acme/shop/web:v1", func(r name.Reference) error { return remote.WriteIndex(r, idx) })

	if err := l.Pushed(ctx, registry.Push{OrgID: org, Repo: "acme/shop/api", Reference: "v1", By: "node1"}); err != nil {
		t.Fatal(err)
	}
	d, _ := amd.Digest()
	row, err := st.RegistryImages.GetByRepoDigest(ctx, "acme/shop/api", d.String())
	if err != nil {
		t.Fatal(err)
	}
	if row.Source != "built" || row.Platforms != "amd64" || row.Size < 64 || row.CreatedBy != "node1" || row.Tag != "v1" {
		t.Fatalf("image row = %+v", row)
	}

	id, _ := idx.Digest()
	if err := l.Pushed(ctx, registry.Push{OrgID: org, Repo: "acme/shop/web", Reference: "v1", Digest: id.String()}); err != nil {
		t.Fatal(err)
	}
	row, err = st.RegistryImages.GetByRepoDigest(ctx, "acme/shop/web", id.String())
	if err != nil || row.Platforms != "amd64,arm64" || row.Size < 128 {
		t.Fatalf("index row = %+v, %v", row, err)
	}

	before, _ := st.RegistryImages.ListAll(ctx)
	if err := l.Pushed(ctx, registry.Push{OrgID: org, Repo: "acme/shop/web", Reference: id.String()}); err != nil {
		t.Fatal(err)
	}
	if after, _ := st.RegistryImages.ListAll(ctx); len(after) != len(before) {
		t.Fatalf("a push by digest recorded: %+v", after)
	}
	if err := l.Pushed(ctx, registry.Push{OrgID: org, Repo: "acme/shop/none", Reference: "v1"}); err == nil {
		t.Error("a push the registry does not have recorded")
	}
}

// An upstream that cannot be reached (down, 503, 429) is answered from our
// own copy of the tag with a warning in the log; a 401 or 404 still fails,
// and so does a tag we never held.
func TestImportUpstreamDown(t *testing.T) {
	local := httptest.NewServer(ggcr.New())
	defer local.Close()
	var status atomic.Int32
	mem := ggcr.New()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s := int(status.Load()); s != 0 {
			w.WriteHeader(s)
			return
		}
		mem.ServeHTTP(w, r)
	}))
	upHost := strings.TrimPrefix(up.URL, "http://")
	st := servicetest.Store(t)
	org := seedOrg(t, st)
	l := registry.New(st.RegistryImages, "").WithBase(local.URL)
	ref, err := name.ParseReference(upHost+"/lib/app:1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, platformImage(t, "amd64")); err != nil {
		t.Fatal(err)
	}
	req := func(log io.Writer, r string) registry.ImportRequest {
		return registry.ImportRequest{OrgID: org, OrgSlug: "acme", Ref: r, Platforms: []string{"amd64"}, Log: log}
	}
	first, err := l.Import(ctx, req(nil, upHost+"/lib/app:1"))
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range []int{503, 429} {
		status.Store(int32(s))
		var log strings.Builder
		got, err := l.Import(ctx, req(&log, upHost+"/lib/app:1"))
		if err != nil || got != first {
			t.Fatalf("%d: import = %+v, %v; want %+v", s, got, err, first)
		}
		if !strings.Contains(log.String(), "could not reach "+upHost) || !strings.Contains(log.String(), "using the copy from") ||
			!strings.Contains(log.String(), "may not be the newest") {
			t.Fatalf("%d: log = %q", s, log.String())
		}
	}
	// a platform we do not hold is no fallback
	status.Store(503)
	r := req(nil, upHost+"/lib/app:1")
	r.Platforms = []string{"arm64"}
	if _, err := l.Import(ctx, r); err == nil {
		t.Error("a missing platform fell back to the copy")
	}
	// a tag we never held
	if _, err := l.Import(ctx, req(nil, upHost+"/lib/other:1")); err == nil {
		t.Error("no local copy, upstream down: import succeeded")
	}
	// an answer is not unreachable
	for _, s := range []int{401, 403, 404} {
		status.Store(int32(s))
		if _, err := l.Import(ctx, req(nil, upHost+"/lib/app:1")); err == nil {
			t.Errorf("%d fell back to the local copy", s)
		}
	}
	// a closed server is
	up.Close()
	var log strings.Builder
	if got, err := l.Import(ctx, req(&log, upHost+"/lib/app:1")); err != nil || got != first || !strings.Contains(log.String(), "could not reach") {
		t.Fatalf("closed server: %+v, %v, %q", got, err, log.String())
	}
}

// A digest pinned ref the registry holds never contacts the upstream.
func TestImportPinnedHeldAsksNobody(t *testing.T) {
	local := httptest.NewServer(ggcr.New())
	defer local.Close()
	var hits atomic.Int32
	mem := ggcr.New()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		mem.ServeHTTP(w, r)
	}))
	defer up.Close()
	upHost := strings.TrimPrefix(up.URL, "http://")
	img := platformImage(t, "amd64")
	ref, err := name.ParseReference(upHost+"/lib/app:1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	st := servicetest.Store(t)
	org := seedOrg(t, st)
	l := registry.New(st.RegistryImages, "").WithBase(local.URL)
	req := registry.ImportRequest{OrgID: org, OrgSlug: "acme", Ref: upHost + "/lib/app@" + d.String(), Platforms: []string{"amd64"}}
	if _, err := l.Import(ctx, req); err != nil {
		t.Fatal(err)
	}
	hits.Store(0)
	if _, err := l.Import(ctx, req); err != nil || hits.Load() != 0 {
		t.Fatalf("held pinned import: err %v, %d requests upstream", err, hits.Load())
	}
	req.Platforms = []string{"arm64"}
	if _, err := l.Import(ctx, req); err == nil || hits.Load() == 0 {
		t.Fatalf("a missing platform: err %v, %d requests upstream", err, hits.Load())
	}
}
