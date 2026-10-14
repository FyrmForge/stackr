package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/imgcopy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/credential"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// copyCall is one imgcopy.Copy the fake saw.
type copyCall struct {
	src, dst  string
	auth      authn.Authenticator
	platforms []string
}

// fakeCopy stands in for the network: it records the call and answers with
// a fixed upstream digest.
func fakeCopy(calls *[]copyCall) func(context.Context, string, string, authn.Authenticator, http.RoundTripper, []string) (imgcopy.Result, error) {
	return func(_ context.Context, src, dst string, srcAuth authn.Authenticator, _ http.RoundTripper, p []string) (imgcopy.Result, error) {
		*calls = append(*calls, copyCall{src, dst, srcAuth, p})
		return imgcopy.Result{Digest: "sha256:up", Platforms: p, Size: 5}, nil
	}
}

// Import resolves the org's credential on the controller, copies for all the
// cluster's CPU types and records the version; the push callback tolerates
// every org, and recordImage writes through the leaf.
func TestImportContract(t *testing.T) {
	ctx := context.Background()
	orch, err := New(
		Config{DataDir: t.TempDir(), SecretsKey: testKey, Conntrack: "/nonexistent"},
		WithDocker(dockerfake.New()),
		WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	must(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	var calls []copyCall
	orch.regs.WithBase("http://127.0.0.1:1").WithCopier(fakeCopy(&calls))

	if _, _, err := orch.Import(ctx, "nope", "postgres:16", "", nil); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("import for an unknown org: %v, want ErrNotFound", err)
	}
	og := store.Org{ID: "o1", Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}
	must(t, orch.store.Orgs.Create(ctx, og))
	cred, err := orch.creds.Create(ctx, og.ID, credential.Spec{Name: "hub", URL: "docker.io", Username: "me", Password: "hunter2"})
	must(t, err)

	local, digest, err := orch.Import(ctx, og.ID, "postgres:16", cred.ID, nil)
	must(t, err)
	if local != "localhost:5000/acme/docker.io/library/postgres:16" || digest != "sha256:up" {
		t.Fatalf("import = %q, %q", local, digest)
	}
	if len(calls) != 1 || calls[0].src != "postgres:16" || calls[0].auth == nil || len(calls[0].platforms) == 0 {
		t.Fatalf("copy = %+v", calls)
	}
	if cfg, _ := calls[0].auth.Authorization(); cfg.Username != "me" || cfg.Password != "hunter2" {
		t.Fatalf("copy credential = %+v", cfg)
	}
	if _, _, err := orch.Import(ctx, og.ID, "postgres:16", "someone-elses", nil); err == nil {
		t.Fatal("an unknown credential id imported")
	}
	if _, _, err := orch.Import(ctx, og.ID, "redis:7", "", []string{"arm64"}); err != nil || calls[1].auth != nil || !slices.Equal(calls[1].platforms, []string{"arm64"}) {
		t.Fatalf("anonymous import with platforms: %v, %+v", err, calls[1:])
	}
	if rows, err := orch.store.RegistryImages.ListByOrg(ctx, og.ID); err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}

	for _, repo := range []string{"acme/shop/web", "ghost/shop/web", regfront.SystemOrg + "/tools/rsync"} {
		orch.registryPushed(ctx, regfront.Pushed{Repo: repo, Reference: "v1", Digest: "sha256:aa", Kind: regfront.KindUser, Subject: "u1"})
	}
	img := store.RegistryImage{OrgID: &og.ID, Repo: "acme/shop/web", Tag: "v1", Digest: "sha256:aa", Source: "built", CreatedAt: time.Now()}
	must(t, orch.recordImage(ctx, img))
}

func TestDockerArch(t *testing.T) {
	for in, want := range map[string]string{
		"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64", "armv7l": "arm", "i686": "386", "riscv64": "riscv64", "": "",
	} {
		if got := dockerArch(in); got != want {
			t.Errorf("dockerArch(%q) = %q, want %q", in, got, want)
		}
	}
}

// The cluster's platforms are the nodes' CPU types, once each, and a node's
// answer is kept for a minute.
func TestClusterPlatforms(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	n.ctlFake.Host.Arch = "x86_64"
	n.wFake.Host.Arch = "x86_64"
	if got := n.orch.clusterPlatforms(ctx); !slices.Equal(got, []string{"amd64"}) {
		t.Fatalf("two amd64 nodes = %v", got)
	}
	n.wFake.Host.Arch = "aarch64"
	if got := n.orch.clusterPlatforms(ctx); !slices.Equal(got, []string{"amd64"}) {
		t.Fatalf("inside the minute = %v", got)
	}
	n.orch.platforms.mu.Lock()
	for id, e := range n.orch.platforms.at {
		e.at = e.at.Add(-2 * platformTTL)
		n.orch.platforms.at[id] = e
	}
	n.orch.platforms.mu.Unlock()
	if got := n.orch.clusterPlatforms(ctx); !slices.Equal(got, []string{"amd64", "arm64"}) {
		t.Fatalf("after the minute = %v", got)
	}
}

// The router pulls an import's registry copy and tags it with the original
// ref; a digest ref is not tagged and is looked up under its registry name.
// Without an import org in the context nothing changes.
func TestRouterPullsImports(t *testing.T) {
	a := dockerfake.New()
	r := NewRouter(map[string]Docker{"n1": a})
	ctx := WithNode(context.Background(), "n1")
	imp := docker.WithImportOrg(ctx, "acme")

	must(t, r.Pull(imp, "nginx:1", "", nil))
	must(t, r.Pull(imp, "ghcr.io/foo/bar@sha256:aa", "", nil))
	_, _ = r.LocalDigest(imp, "ghcr.io/foo/bar@sha256:aa")
	must(t, r.Pull(ctx, "redis:7", "", nil))
	want := []string{
		"Pull(localhost:5000/acme/docker.io/library/nginx:1)",
		"Tag(localhost:5000/acme/docker.io/library/nginx:1, nginx:1)",
		"Pull(localhost:5000/acme/ghcr.io/foo/bar@sha256:aa)",
		"LocalDigest(localhost:5000/acme/ghcr.io/foo/bar@sha256:aa)",
		"Pull(redis:7)",
	}
	if got := callNames(a); !slices.Equal(got, want) {
		t.Fatalf("calls = %v\nwant    %v", got, want)
	}
	if got := r.pinnedLocal(imp, "nginx:1"); got != "nginx:1" {
		t.Errorf("a tag ref was renamed to %q", got)
	}
}

// A deploy of an outside image on a worker (here by a move) imports it first
// and the worker pulls the registry's copy under the original name; the
// controller's own deploy does the same through its loopback registry.
func TestMovePullsImports(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	n.orch.cfg.RegistryImports = true
	var calls []copyCall
	n.orch.regs.WithBase("http://127.0.0.1:1").WithCopier(fakeCopy(&calls))
	n.ctlFake.Host.Arch, n.wFake.Host.Arch = "x86_64", "aarch64"
	n.wFake.RunID = "new"
	n.wFake.Details = map[string]docker.Detail{"new": {Running: true, Networks: map[string]string{n.dev.Network: "10.209.0.2"}}}

	j, err := n.orch.MoveEnv(ctx, &Principal{User: User{ID: "u-admin"}, Access: authz.User{ID: "u-admin", Active: true, Admin: true}}, n.dev.ID, "w1", false)
	must(t, err)
	if j = n.wait(t, j); j.State != job.Done {
		t.Fatalf("move job = %s %q\nw: %v", j.State, j.Error, callNames(n.wFake))
	}
	if len(calls) == 0 || calls[0].src != "nginx:1" || !slices.Equal(calls[0].platforms, []string{"amd64", "arm64"}) {
		t.Fatalf("copies = %+v", calls)
	}
	if !sawCall(n.wFake, "Pull(localhost:5000/acme/docker.io/library/nginx:1)") ||
		!sawCall(n.wFake, "Tag(localhost:5000/acme/docker.io/library/nginx:1, nginx:1)") || sawCall(n.wFake, "Pull(nginx:1)") {
		t.Fatalf("worker calls = %v", callNames(n.wFake))
	}
	rows, err := n.st.RegistryImages.ListAll(ctx)
	if err != nil || len(rows) != 1 || rows[0].Source != "imported" || rows[0].UpstreamRef != "nginx:1" || rows[0].Platforms != "amd64,arm64" {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
}

// Only outside refs go to the registry; a digest ref runs under its registry
// name, a tag ref under the one the user gave; an install without registry
// imports (a fake host) pulls as before.
func TestImportForDeploy(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	var calls []copyCall
	n.orch.regs.WithBase("http://127.0.0.1:1").WithCopier(fakeCopy(&calls))
	og, err := n.orch.store.Orgs.GetBySlug(ctx, "acme")
	must(t, err)
	if _, imported, _ := n.orch.importForDeploy(ctx, og.ID, "nginx:1", "", io.Discard); imported || len(calls) != 0 {
		t.Fatal("imports are off by default")
	}
	n.orch.cfg.RegistryImports = true
	for _, ref := range []string{"localhost:5000/acme/shop/web:abc", "localhost:5000/stackr.system/stackr:1", ""} {
		if _, imported, err := n.orch.importForDeploy(ctx, og.ID, ref, "", io.Discard); imported || err != nil || len(calls) != 0 {
			t.Fatalf("%q was imported: %v", ref, err)
		}
	}
	run, imported, err := n.orch.importForDeploy(ctx, og.ID, "nginx:1", "", io.Discard)
	if err != nil || !imported || run != "nginx:1" {
		t.Fatalf("tag ref: %q %v %v", run, imported, err)
	}
	run, imported, err = n.orch.importForDeploy(ctx, og.ID, "ghcr.io/foo/bar@sha256:abcd", "", io.Discard)
	if err != nil || !imported || run != "localhost:5000/acme/ghcr.io/foo/bar@sha256:up" {
		t.Fatalf("digest ref: %q %v %v", run, imported, err)
	}
}
