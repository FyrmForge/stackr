package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
)

// The controller's localhost:5000 takes only a minted pass, for the repo it
// names: nothing on the host's loopback (a host-network tile) reads or
// writes the registry without one, and a deploy for one org is never minted
// another org's image.
func TestRegistryLocalTakesPassesOnly(t *testing.T) {
	ctx := context.Background()
	// Short: a unix socket path has a 108 byte limit.
	dir, err := os.MkdirTemp("/tmp", "reg")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	orch, err := New(
		Config{DataDir: dir, SecretsKey: testKey, Conntrack: "/nonexistent"},
		WithDocker(dockerfake.New()),
		WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	must(t, err)
	t.Cleanup(func() { _ = orch.Close() })

	// The registry on its socket, where the front reaches it.
	sock := installspec.RegistrySocket(dir)
	must(t, os.MkdirAll(filepath.Dir(sock), 0o700))
	ln, err := net.Listen("unix", sock)
	must(t, err)
	hits := 0
	reg := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = reg.Serve(ln) }()
	t.Cleanup(func() { _ = reg.Close() })

	h := orch.registryLocal()
	do := func(method, path, bearer string) int {
		r := httptest.NewRequest(method, "http://localhost:5000"+path, nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, p := range []string{"/v2/", "/v2/_catalog", "/v2/acme/shop/api/manifests/v1", "/token?scope=repository:acme/shop/api:pull"} {
		if got := do("GET", p, ""); got != http.StatusUnauthorized {
			t.Errorf("GET %s with nothing = %d", p, got)
		}
	}
	if hits != 0 {
		t.Fatalf("the registry was reached %d times unauthenticated", hits)
	}

	mint := registryPass(orch.registryKey(), "test")
	pass := func(ctx context.Context, repo string, push bool) string {
		t.Helper()
		auth, err := mint(ctx, repo, push)
		must(t, err)
		b, err := base64.URLEncoding.DecodeString(auth)
		must(t, err)
		var a struct{ RegistryToken string }
		must(t, json.Unmarshal(b, &a))
		return a.RegistryToken
	}
	pull := pass(ctx, "acme/shop/api", false)
	if got := do("GET", "/v2/acme/shop/api/manifests/v1", pull); got != http.StatusOK || hits != 1 {
		t.Fatalf("pull with its pass = %d, %d hits", got, hits)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/v2/other/shop/api/manifests/v1"},
		{"GET", "/v2/_catalog"},
		{"PUT", "/v2/acme/shop/api/manifests/v1"},
	} {
		if got := do(c.method, c.path, pull); got != http.StatusUnauthorized {
			t.Errorf("%s %s with a pull pass for acme/shop/api = %d", c.method, c.path, got)
		}
	}
	// Nothing writes a system image through a front, not even a pass.
	if got := do("PUT", "/v2/stackr.system/stackr/manifests/v1", pass(ctx, "stackr.system/stackr", true)); got != http.StatusUnauthorized {
		t.Errorf("system push = %d", got)
	}
	if _, err := mint(docker.WithPullOrg(ctx, "other"), "acme/shop/api", false); err == nil {
		t.Error("another org's deploy was minted acme's image")
	}
	if _, err := mint(docker.WithPullOrg(ctx, "other"), "stackr.system/pause", false); err != nil {
		t.Errorf("system image for an org's deploy: %v", err)
	}
}
