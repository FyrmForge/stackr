package registry_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// gcCmd is the sweep's command, as the service runs it in stackr-registry.
var gcCmd = []string{"registry", "garbage-collect", "--delete-untagged", "/etc/distribution/config.yml"}

// realRegistry is a registry:3 with deletes on and the index check off, as
// installspec.Registry runs it, on a random loopback port. The test skips
// without docker and the container is removed afterwards.
type realRegistry struct {
	t    *testing.T
	id   string
	Host string // 127.0.0.1:port
}

func startRegistry(t *testing.T) *realRegistry {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if err := exec.Command("docker", "image", "inspect", "registry:3").Run(); err != nil {
		t.Skip("registry:3 is not pulled")
	}
	out, err := exec.Command("docker", "run", "-d", "-p", "127.0.0.1::5000",
		"-e", "REGISTRY_STORAGE_DELETE_ENABLED=true",
		"-e", "REGISTRY_VALIDATION_MANIFESTS_INDEXES_PLATFORMS=none",
		"registry:3").Output()
	if err != nil {
		t.Skipf("docker run: %v", err)
	}
	r := &realRegistry{t: t, id: strings.TrimSpace(string(out))}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", r.id).Run() })
	port, err := exec.Command("docker", "port", r.id, "5000/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	r.Host = strings.TrimSpace(strings.Split(string(port), "\n")[0])
	for range 50 {
		if resp, err := http.Get("http://" + r.Host + "/v2/"); err == nil {
			_ = resp.Body.Close()
			return r
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("registry did not come up")
	return nil
}

func (r *realRegistry) URL() string { return "http://" + r.Host }

func (r *realRegistry) gc() string {
	r.t.Helper()
	out, err := exec.Command("docker", append([]string{"exec", r.id}, gcCmd...)...).CombinedOutput()
	if err != nil {
		r.t.Fatalf("garbage-collect: %v\n%s", err, out)
	}
	return string(out)
}

// status is the HTTP status of a HEAD on path.
func (r *realRegistry) status(path string) int {
	r.t.Helper()
	req, _ := http.NewRequest(http.MethodHead, r.URL()+path, nil)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// onDisk says whether the blob digest is still in the registry's storage.
func (r *realRegistry) onDisk(digest string) bool {
	hex := strings.TrimPrefix(digest, "sha256:")
	return exec.Command("docker", "exec", r.id, "ls", "/var/lib/registry/docker/registry/v2/blobs/sha256/"+hex[:2]+"/"+hex).Run() == nil
}

// push writes a random image (one layer) to repo at ref (a tag, or @digest
// when withDigest is set) and returns it.
func (r *realRegistry) push(repo, tag string) v1.Image {
	r.t.Helper()
	img, err := random.Image(2048, 1)
	if err != nil {
		r.t.Fatal(err)
	}
	r.write(repo+":"+tag, img)
	return img
}

func (r *realRegistry) write(ref string, img v1.Image) {
	r.t.Helper()
	n, err := name.ParseReference(r.Host+"/"+ref, name.Insecure)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := remote.Write(n, img); err != nil {
		r.t.Fatal(err)
	}
}

// blobs are the config and layer paths of img under repo.
func blobs(t *testing.T, repo string, img v1.Image) []string {
	t.Helper()
	m, err := img.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	out := []string{fmt.Sprintf("/v2/%s/blobs/%s", repo, m.Config.Digest)}
	for _, l := range m.Layers {
		out = append(out, fmt.Sprintf("/v2/%s/blobs/%s", repo, l.Digest))
	}
	return out
}

func digestOf(t *testing.T, img v1.Image) string {
	t.Helper()
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d.String()
}

// Clean against the real registry: the newest keep and what inUse names stay
// (a tag that shares a digest with an in-use one too), the rest goes with its
// row, a hand-pushed digest with no row counts as oldest, and the next garbage
// collect frees exactly the deleted images' blobs.
func TestCleanRealRegistry(t *testing.T) {
	r := startRegistry(t)
	st := servicetest.Store(t)
	org := seedOrg(t, st)
	l := registry.New(st.RegistryImages).WithBase(r.URL())
	const repo = "acme/docker.io/library/app"

	t0 := time.Now().Add(-time.Hour)
	imgs := map[string]v1.Image{}
	for i, tag := range []string{"t1", "t2", "t3", "hand"} {
		img := r.push(repo, tag)
		imgs[tag] = img
		if tag == "hand" {
			continue // no row
		}
		err := l.Record(ctx, store.RegistryImage{
			OrgID: &org, Repo: repo, Tag: tag, Digest: digestOf(t, img), Source: registry.SourceImported,
			Size: int64(100 * (i + 1)), CreatedAt: t0.Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// A second tag on t1's image, which an env uses.
	r.write(repo+":latest", imgs["t1"])
	inUse := func(_ context.Context, rp, d string) (bool, error) {
		return rp == repo && d == digestOf(t, imgs["t1"]), nil
	}

	got, err := l.Clean(ctx, 1, inUse)
	if err != nil || len(got.Errors) != 0 {
		t.Fatalf("clean = %+v, %v", got, err)
	}
	if got.Deleted != 2 || got.Freed != 200 { // t2 (row, 200) and hand (no row)
		t.Fatalf("clean = %+v, want 2 deleted, 200 freed", got)
	}
	for tag, want := range map[string]int{"t1": 200, "latest": 200, "t2": 404, "t3": 200, "hand": 404} {
		if code := r.status("/v2/" + repo + "/manifests/" + tag); code != want {
			t.Errorf("%s = %d, want %d", tag, code, want)
		}
	}
	rows, err := st.RegistryImages.ListByRepo(ctx, repo)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}

	t.Log(r.gc())
	for _, tag := range []string{"t2", "hand"} {
		for _, p := range blobs(t, repo, imgs[tag]) {
			if r.onDisk(p[strings.LastIndex(p, "/")+1:]) {
				t.Errorf("%s: blob %s survived the collect", tag, p)
			}
		}
	}
	for _, tag := range []string{"t1", "t3"} {
		for _, p := range blobs(t, repo, imgs[tag]) {
			if !r.onDisk(p[strings.LastIndex(p, "/")+1:]) {
				t.Errorf("%s: blob %s was collected", tag, p)
			}
		}
	}

	// Resolve: a tag to its digest, "" when absent.
	if d, err := l.Resolve(ctx, repo, "t3"); err != nil || d != digestOf(t, imgs["t3"]) {
		t.Errorf("resolve t3 = %q, %v", d, err)
	}
	if d, err := l.Resolve(ctx, repo, "nope"); err != nil || d != "" {
		t.Errorf("resolve nope = %q, %v", d, err)
	}

	// An inUse that cannot tell protects the version and is noted.
	got, err = l.Clean(ctx, 0, func(context.Context, string, string) (bool, error) { return false, errors.New("boom") })
	if err != nil || got.Deleted != 0 || len(got.Errors) == 0 {
		t.Errorf("unknown use = %+v, %v", got, err)
	}
	if code := r.status("/v2/" + repo + "/manifests/t3"); code != 200 {
		t.Errorf("t3 = %d after an inUse error", code)
	}
}

// The gate is shut for the whole sweep and open again whatever it returns.
func TestWhileGated(t *testing.T) {
	l := registry.New(nil)
	err := l.WhileGated(ctx, func(context.Context) error {
		if !l.Gate().Closed() {
			t.Error("gate open inside the sweep")
		}
		if l.WhileGated(ctx, func(context.Context) error { return nil }) != registry.ErrSweeping {
			t.Error("a second sweep ran")
		}
		waitCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		if l.Wait(waitCtx) == nil {
			t.Error("Wait returned inside the sweep")
		}
		return errors.New("gc failed")
	})
	if err == nil || l.Gate().Closed() {
		t.Fatalf("after a failed sweep: err %v, closed %v", err, l.Gate().Closed())
	}
	if err := l.Wait(ctx); err != nil {
		t.Errorf("Wait on an open gate = %v", err)
	}
	func() { // a panic also opens it
		defer func() { _ = recover() }()
		_ = l.WhileGated(ctx, func(context.Context) error { panic("x") })
	}()
	if l.Gate().Closed() {
		t.Error("gate still shut after a panic")
	}
}

// The first proof of the cleanup: against the real registry, a garbage collect
// with --delete-untagged keeps everything behind a tagged index (even one whose
// arm64 child was never pushed) and a tagged image, and frees a deleted
// image's blobs.
func TestRealRegistryGarbageCollect(t *testing.T) {
	r := startRegistry(t)
	const repo = "acme/docker.io/library/app"
	const soloRepo = "acme/docker.io/library/solo"

	amd, err := random.Image(2048, 1)
	if err != nil {
		t.Fatal(err)
	}
	amdDigest := digestOf(t, amd)
	r.write(repo+"@"+amdDigest, amd) // by digest only, as an import writes a child
	// The index names the amd64 child and an arm64 one that is absent.
	amdSize, _ := amd.Size()
	idx, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{
			{"mediaType": "application/vnd.docker.distribution.manifest.v2+json", "digest": amdDigest, "size": amdSize,
				"platform": map[string]string{"os": "linux", "architecture": "amd64"}},
			{"mediaType": "application/vnd.docker.distribution.manifest.v2+json", "digest": "sha256:" + strings.Repeat("ab", 32), "size": 1234,
				"platform": map[string]string{"os": "linux", "architecture": "arm64"}},
		},
	})
	sum := sha256.Sum256(idx)
	idxDigest := "sha256:" + hex.EncodeToString(sum[:])
	req, _ := http.NewRequest(http.MethodPut, r.URL()+"/v2/"+repo+"/manifests/16", bytes.NewReader(idx))
	req.Header.Set("Content-Type", "application/vnd.oci.image.index.v1+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("index with an absent child = %d", resp.StatusCode)
	}
	solo := r.push(soloRepo, "1")
	soloDigest := digestOf(t, solo)

	t.Log(r.gc())
	for _, p := range []string{
		"/v2/" + repo + "/manifests/16",
		"/v2/" + repo + "/manifests/" + idxDigest,
		"/v2/" + repo + "/manifests/" + amdDigest,
		"/v2/" + soloRepo + "/manifests/1",
		"/v2/" + soloRepo + "/manifests/" + soloDigest,
	} {
		if got := r.status(p); got != 200 {
			t.Errorf("after gc --delete-untagged: HEAD %s = %d", p, got)
		}
	}
	for _, p := range append(blobs(t, repo, amd), blobs(t, soloRepo, solo)...) {
		if got := r.status(p); got != 200 {
			t.Errorf("after gc --delete-untagged: blob %s = %d", p, got)
		}
	}

	// Delete the single image by digest: its blobs go at the next collect.
	req, _ = http.NewRequest(http.MethodDelete, r.URL()+"/v2/"+soloRepo+"/manifests/"+soloDigest, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	t.Log(r.gc())
	for _, p := range blobs(t, soloRepo, solo) {
		if r.onDisk(p[strings.LastIndex(p, "/")+1:]) {
			t.Errorf("deleted image's blob %s is still on disk after gc", p)
		}
	}
	// Pushing the same image again must work: the registry's descriptor
	// cache must not claim blobs the collect removed.
	r.write(soloRepo+":2", solo)
	for _, p := range append(blobs(t, repo, amd), "/v2/"+repo+"/manifests/16", "/v2/"+repo+"/manifests/"+amdDigest) {
		if got := r.status(p); got != 200 {
			t.Errorf("%s = %d after the other image went", p, got)
		}
	}
}
