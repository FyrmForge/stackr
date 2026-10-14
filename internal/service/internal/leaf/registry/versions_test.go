package registry_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

const (
	repoBuilt  = "acme/shop/api"
	repoImport = "acme/docker.io/library/postgres"
	dgBuilt    = "sha256:b1"
	dgIndex    = "sha256:i1"
)

// fakeRegistry holds one built single-arch image (tag 1) and one imported
// multi-arch index (tags 16 and latest).
func fakeRegistry(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	deleted := &[]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/_catalog", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"repositories": []string{repoBuilt, repoImport}})
	})
	mux.HandleFunc("/v2/"+repoBuilt+"/tags/list", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"1"}})
	})
	mux.HandleFunc("/v2/"+repoImport+"/tags/list", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"16", "latest"}})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodDelete && strings.Contains(p, "/manifests/"):
			mu.Lock()
			*deleted = append(*deleted, strings.TrimPrefix(p, "/v2/"))
			mu.Unlock()
			if strings.HasSuffix(p, "gone") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		case strings.HasPrefix(p, "/v2/"+repoBuilt+"/manifests/"):
			w.Header().Set("Docker-Content-Digest", dgBuilt)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"config": map[string]string{"digest": "sha256:cfg"},
				"layers": []map[string]int64{{"size": 60}, {"size": 40}},
			})
		case strings.HasPrefix(p, "/v2/"+repoBuilt+"/blobs/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"created": time.Now().Add(-time.Hour), "architecture": "amd64"})
		case strings.HasPrefix(p, "/v2/"+repoImport+"/manifests/"):
			w.Header().Set("Docker-Content-Digest", dgIndex)
			_ = json.NewEncoder(w).Encode(map[string]any{"manifests": []map[string]any{
				{"digest": "sha256:c1", "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
				{"digest": "sha256:c2", "platform": map[string]string{"os": "unknown", "architecture": "unknown"}},
			}})
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, deleted
}

// An imported index shows in the listing as latest (its row says when), and
// a version lists its tags with the row's detail; an image with no row shows
// what the manifest says.
func TestVersionsAndListIndex(t *testing.T) {
	srv, _ := fakeRegistry(t)
	st := servicetest.Store(t)
	org := seedOrg(t, st)
	l := registry.New(st.RegistryImages, "").WithBase(srv.URL)
	arrived := time.Now().Add(-2 * time.Hour).UTC()
	if err := l.Record(ctx, store.RegistryImage{
		OrgID: &org, Repo: repoImport, Tag: "16", Digest: dgIndex, Platforms: "amd64",
		Source: registry.SourceImported, UpstreamRef: "postgres:16", Size: 4096, CreatedBy: "u1", CreatedAt: arrived,
	}); err != nil {
		t.Fatal(err)
	}

	repos, err := l.List(ctx, "acme")
	if err != nil || len(repos) != 2 {
		t.Fatalf("list = %+v, %v", repos, err)
	}
	pg := repos[0]
	if pg.Name != repoImport || pg.Tags != 2 || pg.Latest == "" || pg.Size != 4096 || !pg.LatestAt.Equal(arrived) {
		t.Fatalf("imported repo = %+v", pg)
	}

	vs, err := l.Versions(ctx, repoImport)
	if err != nil || len(vs) != 2 {
		t.Fatalf("versions = %+v, %v", vs, err)
	}
	v := vs[0]
	if v.Digest != dgIndex || v.Source != "imported" || v.UpstreamRef != "postgres:16" || v.Size != 4096 ||
		len(v.Platforms) != 1 || v.Platforms[0] != "amd64" || v.CreatedBy != "u1" {
		t.Fatalf("version = %+v", v)
	}

	vs, err = l.Versions(ctx, repoBuilt)
	if err != nil || len(vs) != 1 {
		t.Fatalf("built versions = %+v, %v", vs, err)
	}
	if v := vs[0]; v.Tag != "1" || v.Digest != dgBuilt || v.Size != 100 || v.Source != "" || v.CreatedAt.IsZero() ||
		len(v.Platforms) != 1 || v.Platforms[0] != "amd64" {
		t.Fatalf("built version = %+v", v)
	}
}

// Delete drops the manifest and the row together; an image already gone is
// not an error, and a dead registry keeps the row.
func TestDelete(t *testing.T) {
	srv, deleted := fakeRegistry(t)
	st := servicetest.Store(t)
	org := seedOrg(t, st)
	l := registry.New(st.RegistryImages, "").WithBase(srv.URL)
	img := store.RegistryImage{OrgID: &org, Repo: repoImport, Tag: "16", Digest: dgIndex, Source: registry.SourceImported}
	if err := l.Record(ctx, img); err != nil {
		t.Fatal(err)
	}
	if err := registry.New(st.RegistryImages, "").WithBase("http://127.0.0.1:1").Delete(ctx, repoImport, dgIndex); err == nil {
		t.Error("a dead registry deleted")
	}
	if _, err := st.RegistryImages.GetByRepoDigest(ctx, repoImport, dgIndex); err != nil {
		t.Fatalf("row lost with the registry down: %v", err)
	}
	if err := l.Delete(ctx, repoImport, dgIndex); err != nil {
		t.Fatal(err)
	}
	if len(*deleted) != 1 || (*deleted)[0] != repoImport+"/manifests/"+dgIndex {
		t.Fatalf("deleted %v", *deleted)
	}
	if _, err := st.RegistryImages.GetByRepoDigest(ctx, repoImport, dgIndex); err == nil {
		t.Error("row stayed")
	}
	img.Digest = "sha256:gone"
	if err := l.Record(ctx, img); err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(ctx, repoImport, "sha256:gone"); err != nil {
		t.Fatalf("already gone = %v", err)
	}
}

// Usage adds the rows per org and skips a row whose repo the registry lost.
func TestUsage(t *testing.T) {
	srv, _ := fakeRegistry(t)
	st := servicetest.Store(t)
	org := seedOrg(t, st)
	l := registry.New(st.RegistryImages, "").WithBase(srv.URL)
	for _, r := range []store.RegistryImage{
		{OrgID: &org, Repo: repoImport, Tag: "16", Digest: "sha256:1", Source: "built", Size: 10},
		{OrgID: &org, Repo: repoImport, Tag: "15", Digest: "sha256:2", Source: "built", Size: 5},
		{OrgID: &org, Repo: "acme/shop/gone", Tag: "1", Digest: "sha256:3", Source: "built", Size: 99},
		{Repo: "stackr.system/x", Tag: "1", Digest: "sha256:4", Source: "built", Size: 1},
	} {
		if err := l.Record(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.Usage(ctx)
	if err != nil || len(got) != 1 || got[0].Org != "acme" || got[0].Versions != 2 || got[0].Bytes != 15 {
		t.Fatalf("usage = %+v, %v", got, err)
	}
}
