package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A version is used by the envs whose current release pins it, a built image
// by its ref and a pulled one by its digest; deleting a used version is
// refused naming the envs, and a forced or unused one goes with its row.
func TestRegistryVersionsUseAndDelete(t *testing.T) {
	ctx := context.Background()
	orch, err := New(
		Config{DataDir: t.TempDir(), SecretsKey: testKey, Conntrack: "/nonexistent"},
		WithDocker(dockerfake.New()),
		WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	must(t, err)
	t.Cleanup(func() { _ = orch.Close() })

	const pg, api = "acme/docker.io/library/postgres", "acme/shop/api"
	var deleted []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v2/")
		switch {
		case r.Method == http.MethodDelete:
			deleted = append(deleted, p)
			w.WriteHeader(http.StatusAccepted)
		case strings.HasSuffix(p, "/tags/list"):
			tags := map[string][]string{pg + "/tags/list": {"16", "latest"}, api + "/tags/list": {"v1", "v2"}}[p]
			_ = json.NewEncoder(w).Encode(map[string]any{"tags": tags})
		case strings.HasPrefix(p, pg+"/manifests/"):
			w.Header().Set("Docker-Content-Digest", "sha256:pg")
			_ = json.NewEncoder(w).Encode(map[string]any{"manifests": []any{}})
		case strings.HasPrefix(p, api+"/manifests/"):
			w.Header().Set("Docker-Content-Digest", "sha256:api-"+p[strings.LastIndex(p, "/")+1:])
			_ = json.NewEncoder(w).Encode(map[string]any{"config": map[string]string{"digest": "sha256:c"}})
		case strings.Contains(p, "/blobs/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"created": time.Now()})
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	orch.regs.WithBase(srv.URL)

	now := time.Now()
	og := store.Org{ID: "o1", Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: now}
	must(t, orch.store.Orgs.Create(ctx, og))
	must(t, orch.store.Stacks.Create(ctx, store.Stack{ID: "s1", OrgID: og.ID, Name: "shop", Slug: "shop", Settings: "{}", CreatedAt: now}))
	must(t, orch.store.Releases.Create(ctx, store.Release{ID: "r1", StackID: "s1", Number: 1, CreatedAt: now}))
	rel := "r1"
	must(t, orch.store.Environments.Create(ctx, store.Environment{
		ID: "e1", StackID: "s1", Name: "dev", Slug: "dev", Type: "static", FromKind: "branch", FromBranch: "main", Settings: "{}", ReleaseID: &rel, CreatedAt: now,
	}))
	must(t, orch.store.Images.Create(ctx, store.Image{ID: "i1", Ref: "localhost:5000/" + api + ":v1", CreatedAt: now}))
	img := "i1"
	must(t, orch.store.ReleaseTiles.Create(ctx, store.ReleaseTile{ID: "rt1", ReleaseID: "r1", Slug: "api", ImageID: &img}))
	must(t, orch.store.ReleaseTiles.Create(ctx, store.ReleaseTile{ID: "rt2", ReleaseID: "r1", Slug: "db", Repo: "postgres", Digest: "sha256:pg"}))
	for _, r := range []store.RegistryImage{
		{OrgID: &og.ID, Repo: pg, Tag: "16", Digest: "sha256:pg", Source: "imported", UpstreamRef: "postgres:16", CreatedAt: now},
		{OrgID: &og.ID, Repo: api, Tag: "v1", Digest: "sha256:api-v1", Source: "built", CreatedAt: now},
		{OrgID: &og.ID, Repo: api, Tag: "v2", Digest: "sha256:api-v2", Source: "built", CreatedAt: now},
	} {
		must(t, orch.recordImage(ctx, r))
	}

	vs, err := orch.RegistryVersions(ctx, og.ID, api)
	if err != nil || len(vs) != 2 {
		t.Fatalf("versions = %+v, %v", vs, err)
	}
	for _, v := range vs {
		if used := len(v.UsedBy) == 1 && v.UsedBy[0] == "shop/dev"; used != (v.Tag == "v1") {
			t.Errorf("%s used by %v", v.Tag, v.UsedBy)
		}
	}
	if vs, err = orch.RegistryVersions(ctx, og.ID, pg); err != nil || len(vs) != 2 || len(vs[0].UsedBy) != 1 || len(vs[1].UsedBy) != 1 {
		t.Fatalf("a digest pin must mark every tag on the digest: %+v, %v", vs, err)
	}
	if _, err := orch.RegistryVersions(ctx, og.ID, "other/shop/api"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("another org's repo = %v, want not found", err)
	}

	err = orch.DeleteRegistryVersion(ctx, og.ID, api, "v1", false)
	if !isConflict(err) || !strings.Contains(err.Error(), "shop/dev") || len(deleted) != 0 {
		t.Fatalf("used delete = %v, deleted %v", err, deleted)
	}
	if err := orch.DeleteRegistryVersion(ctx, og.ID, api, "nope", false); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("unknown tag = %v", err)
	}
	must(t, orch.DeleteRegistryVersion(ctx, og.ID, api, "v2", false))
	if len(deleted) != 1 || deleted[0] != api+"/manifests/sha256:api-v2" {
		t.Fatalf("deleted %v", deleted)
	}
	if _, err := orch.store.RegistryImages.GetByRepoDigest(ctx, api, "sha256:api-v2"); err == nil {
		t.Error("row of the deleted version stayed")
	}
	must(t, orch.DeleteRegistryVersion(ctx, og.ID, api, "v1", true))
	if len(deleted) != 2 {
		t.Fatalf("forced delete: %v", deleted)
	}
}
