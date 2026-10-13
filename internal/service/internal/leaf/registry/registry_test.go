package registry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func seedOrg(t *testing.T, st *store.Store) string {
	t.Helper()
	id := uuid.NewString()
	if err := st.Orgs.Create(ctx, store.Org{
		ID: id, Name: id, Slug: id[:8], EnvColors: "{}", Settings: "{}", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// The listing keeps the org's repositories, counts tags and takes the newest
// build as latest.
func TestList(t *testing.T) {
	built := map[string]time.Time{
		"acme/shop/api:old": time.Now().Add(-48 * time.Hour),
		"acme/shop/api:new": time.Now().Add(-time.Hour),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/_catalog", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"repositories": []string{"acme/shop/api", "other/x/y"}})
	})
	mux.HandleFunc("/v2/acme/shop/api/tags/list", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"old", "new"}})
	})
	mux.HandleFunc("/v2/other/x/y/tags/list", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"1"}})
	})
	mux.HandleFunc("/v2/acme/shop/api/manifests/", func(w http.ResponseWriter, r *http.Request) {
		tag := r.URL.Path[len("/v2/acme/shop/api/manifests/"):]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"config": map[string]string{"digest": "sha256:" + tag},
			"layers": []map[string]int64{{"size": 100}, {"size": 20}},
		})
	})
	mux.HandleFunc("/v2/acme/shop/api/blobs/", func(w http.ResponseWriter, r *http.Request) {
		tag := r.URL.Path[len("/v2/acme/shop/api/blobs/sha256:"):]
		_ = json.NewEncoder(w).Encode(map[string]any{"created": built["acme/shop/api:"+tag]})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	l := registry.New(nil).WithBase(srv.URL)
	got, err := l.List(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "acme/shop/api" || got[0].Tags != 2 || got[0].Latest != "new" || got[0].Size != 120 {
		t.Fatalf("repositories = %+v", got)
	}
	if all, err := l.List(ctx, ""); err != nil || len(all) != 2 {
		t.Fatalf("every org = %+v, %v", all, err)
	}
	if _, err := registry.New(nil).WithBase("http://127.0.0.1:1").List(ctx, "acme"); err == nil {
		t.Error("a dead registry listed")
	}
}

func TestLocal(t *testing.T) {
	if got := registry.New(nil).Local("acme/docker.io/library/postgres", "16"); got != "localhost:5000/acme/docker.io/library/postgres:16" {
		t.Fatalf("local = %s", got)
	}
}

// Record writes a version once; a repeat updates platforms and size only and
// keeps who, when, source and the first tag.
func TestRecord(t *testing.T) {
	st := servicetest.Store(t)
	l := registry.New(st.RegistryImages)
	org := seedOrg(t, st)
	t0 := time.Date(2026, 10, 13, 4, 15, 0, 0, time.UTC)

	first := store.RegistryImage{
		OrgID: &org, Repo: "acme/docker.io/library/postgres", Tag: "16", Digest: "sha256:aa",
		Platforms: registry.JoinPlatforms([]string{"amd64"}), Source: registry.SourceImported,
		UpstreamRef: "postgres:16", Size: 100, CreatedBy: "u1", CreatedAt: t0,
	}
	if err := l.Record(ctx, first); err != nil {
		t.Fatal(err)
	}
	again := first
	again.Tag, again.Platforms, again.Size, again.CreatedBy, again.CreatedAt = "16.1", "amd64,arm64", 250, "u2", t0.Add(time.Hour)
	if err := l.Record(ctx, again); err != nil {
		t.Fatal(err)
	}
	rows, err := st.RegistryImages.ListByRepo(ctx, first.Repo)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	r := rows[0]
	if r.ID == "" || r.Tag != "16" || r.Platforms != "amd64,arm64" || r.Size != 250 || r.CreatedBy != "u1" || !r.CreatedAt.Equal(t0) || r.Source != "imported" {
		t.Fatalf("row = %+v", r)
	}

	// stackr's own image: no org
	sys := store.RegistryImage{Repo: "stackr.system/stackr", Tag: "0.6.0", Digest: "sha256:bb", Source: registry.SourceSystem, OrgID: ptr("")}
	if err := l.Record(ctx, sys); err != nil {
		t.Fatal(err)
	}
	if got, err := st.RegistryImages.GetByRepoDigest(ctx, sys.Repo, sys.Digest); err != nil || got.OrgID != nil || got.CreatedAt.IsZero() {
		t.Fatalf("system row = %+v, %v", got, err)
	}
	if err := l.Record(ctx, store.RegistryImage{Repo: "x"}); err == nil {
		t.Error("a version with no digest recorded")
	}
}

func ptr[T any](v T) *T { return &v }

func TestPlatforms(t *testing.T) {
	if got := registry.JoinPlatforms([]string{"arm64", "amd64", "arm64"}); got != "amd64,arm64" {
		t.Fatalf("join = %q", got)
	}
	if got := registry.SplitPlatforms("amd64,arm64"); len(got) != 2 || got[1] != "arm64" || registry.SplitPlatforms("") != nil {
		t.Fatalf("split = %v", got)
	}
}
