package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Only the envs on the controller are named, and only once a worker exists.
func TestControllerEnvs(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	name, envs, err := n.orch.ControllerEnvs(ctx, n.stack)
	must(t, err)
	if name != n.ctl.Name || len(envs) != 1 || envs[0].ID != n.dev.ID {
		t.Fatalf("controller envs = %q %+v, want only dev on %q", name, envs, n.ctl.Name)
	}
	must(t, n.st.Nodes.Delete(ctx, "w1")) // back to one node: no warning
	if _, envs, _ = n.orch.ControllerEnvs(ctx, n.stack); len(envs) != 0 {
		t.Errorf("one node still warns: %+v", envs)
	}
}

// The registry list keeps the org's repositories, counts tags and takes the
// newest build as latest.
func TestListRepositories(t *testing.T) {
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

	got, err := listRepositories(context.Background(), srv.URL, "acme")
	must(t, err)
	if len(got) != 1 || got[0].Name != "acme/shop/api" || got[0].Tags != 2 || got[0].Latest != "new" || got[0].Size != 120 {
		t.Fatalf("repositories = %+v", got)
	}
	if _, err := listRepositories(context.Background(), "http://127.0.0.1:1", "acme"); err == nil {
		t.Error("a dead registry listed")
	}
}
