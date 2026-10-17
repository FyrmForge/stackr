package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
	"github.com/FyrmForge/stackr/internal/service/internal/imgcopy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/credential"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
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
		if got := docker.Arch(in); got != want {
			t.Errorf("docker.Arch(%q) = %q, want %q", in, got, want)
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

// A deploy of an outside image on a worker (here by a move) imports it first
// and the worker pulls the org's registry copy at its digest, never under the
// outside name.
func TestMovePullsImports(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	n.orch.cfg.RegistryImports = true
	var calls []copyCall
	n.orch.regs.WithBase("http://127.0.0.1:1").WithCopier(fakeCopy(&calls))
	n.ctlFake.Host.Arch, n.wFake.Host.Arch = "x86_64", "aarch64"
	n.wFake.RunID = "new"
	n.wFake.Details = map[string]docker.Detail{"new": {Running: true, Networks: map[string]string{n.dev.Network: "10.209.0.2"}}}

	j, err := n.startMove(&Principal{User: User{ID: "u-admin"}, Access: authz.User{ID: "u-admin", Active: true, Admin: true}}, n.dev.ID, "w1", false)
	must(t, err)
	if j = n.wait(t, j); j.State != job.Done {
		t.Fatalf("move job = %s %q\nw: %v", j.State, j.Error, callNames(n.wFake))
	}
	if len(calls) == 0 || calls[0].src != "nginx:1" || !slices.Equal(calls[0].platforms, []string{"amd64", "arm64"}) {
		t.Fatalf("copies = %+v", calls)
	}
	if !sawCall(n.wFake, "Pull(localhost:5000/acme/docker.io/library/nginx@sha256:up)") ||
		sawCall(n.wFake, "Tag(localhost:5000/acme/docker.io/library/nginx@sha256:up, nginx:1)") || sawCall(n.wFake, "Pull(nginx:1)") {
		t.Fatalf("worker calls = %v", callNames(n.wFake))
	}
	rows, err := n.st.RegistryImages.ListAll(ctx)
	if err != nil || len(rows) != 1 || rows[0].Source != "imported" || rows[0].UpstreamRef != "nginx:1" || rows[0].Platforms != "amd64,arm64" {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
}

// Only outside refs go to the registry; a tag or digest ref runs as the org's
// registry copy at its digest; an install without registry imports (a fake
// host) pulls as before.
func TestImportForDeploy(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	var calls []copyCall
	n.orch.regs.WithBase("http://127.0.0.1:1").WithCopier(fakeCopy(&calls))
	og, err := n.orch.store.Orgs.GetBySlug(ctx, "acme")
	must(t, err)
	if _, imported, _ := n.orch.importForDeploy(ctx, og.ID, "nginx:1", store.Credential{}, io.Discard); imported || len(calls) != 0 {
		t.Fatal("imports are off by default")
	}
	n.orch.cfg.RegistryImports = true
	for _, ref := range []string{"localhost:5000/acme/shop/web:abc", "localhost:5000/stackr.system/stackr:1", ""} {
		if _, imported, err := n.orch.importForDeploy(ctx, og.ID, ref, store.Credential{}, io.Discard); imported || err != nil || len(calls) != 0 {
			t.Fatalf("%q was imported: %v", ref, err)
		}
	}
	// Another org's image is refused by name, whether or not the node holds it.
	must(t, n.orch.SetSetting(ctx, "panel_domain", "panel.test"))
	for _, ref := range []string{"localhost:5000/zed/shop/web@sha256:abc", "localhost:5000/zed/docker.io/library/redis:7", "registry.panel.test/zed/shop/web:abc"} {
		if _, _, err := n.orch.importForDeploy(ctx, og.ID, ref, store.Credential{}, io.Discard); err == nil {
			t.Errorf("%q: another org's image was accepted", ref)
		}
	}
	run, imported, err := n.orch.importForDeploy(ctx, og.ID, "nginx:1", store.Credential{}, io.Discard)
	if err != nil || !imported || run != "localhost:5000/acme/docker.io/library/nginx@sha256:up" {
		t.Fatalf("tag ref: %q %v %v", run, imported, err)
	}
	run, imported, err = n.orch.importForDeploy(ctx, og.ID, "ghcr.io/foo/bar@sha256:abcd", store.Credential{}, io.Discard)
	if err != nil || !imported || run != "localhost:5000/acme/ghcr.io/foo/bar@sha256:up" {
		t.Fatalf("digest ref: %q %v %v", run, imported, err)
	}
}

// Two orgs deploy the same outside tag on one node while it means different
// images to each: each container runs its own org's copy by digest, the
// outside name is never tagged on the node, and each label pins its digest.
func TestImportsDoNotShareANameAcrossOrgs(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	n.orch.cfg.RegistryImports = true
	n.orch.regs.WithBase("http://127.0.0.1:1").WithCopier(func(_ context.Context, _, dst string, _ authn.Authenticator, _ http.RoundTripper, p []string) (imgcopy.Result, error) {
		org := strings.Split(dst, "/")[1]
		return imgcopy.Result{Digest: "sha256:" + org, Platforms: p}, nil
	})
	zed, stack := uuid.NewString(), uuid.NewString()
	must(t, n.st.Orgs.Create(ctx, store.Org{ID: zed, Name: "zed", Slug: "zed", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}))
	must(t, n.st.Stacks.Create(ctx, store.Stack{ID: stack, OrgID: zed, Name: "sh", Slug: "sh", Settings: "{}", CreatedAt: time.Now()}))
	env, err := n.orch.CreateEnv(ctx, stack, "dev", EnvSpec{NodeID: "w1", Type: environment.Static, FromKind: environment.FromBranch, FromBranch: "main"})
	must(t, err)
	zt, err := n.orch.CreateTile(ctx, Tile{StackID: stack, EnvironmentID: env.ID, Name: "web", Kind: tile.Image, ImageRef: "nginx:1", ContainerPort: 80})
	must(t, err)

	want := map[string]string{} // image -> label
	for _, c := range []struct {
		org string
		tl  Tile
		net string
	}{{"acme", n.edgeT, n.edge.Network}, {"zed", zt, env.Network}} {
		run := "localhost:5000/" + c.org + "/docker.io/library/nginx@sha256:" + c.org
		n.wFake.Digests = map[string]string{run: "sha256:" + c.org}
		n.wFake.RunID = "c-" + c.org
		n.wFake.Details = map[string]docker.Detail{n.wFake.RunID: {Running: true, Networks: map[string]string{c.net: "10.209.0.2"}}}
		j, err := n.orch.Deploy(ctx, c.tl.ID)
		must(t, err)
		if j = n.wait(t, j); j.State != job.Done {
			t.Fatalf("%s deploy = %s %q", c.org, j.State, j.Error)
		}
		want[run] = "nginx@sha256:" + c.org
	}
	got := map[string]string{}
	for _, s := range n.wFake.Specs {
		if l, ok := s.Labels[deploy.LabelRef]; ok {
			got[s.Image] = l
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("containers = %v\nwant %v", got, want)
	}
	for _, c := range callNames(n.wFake) {
		if strings.HasPrefix(c, "Tag(") || c == "Pull(nginx:1)" {
			t.Errorf("the outside name was used on the node: %s", c)
		}
	}

	// The registry sweep keeps both versions: each env's release pins its own.
	reg := httptest.NewServer(http.NotFoundHandler())
	defer reg.Close()
	n.orch.regs.WithBase(reg.URL)
	used, err := n.orch.registryInUse(ctx)
	must(t, err)
	if !used["sha256:acme"] || !used["sha256:zed"] {
		t.Errorf("in use = %v", used)
	}

	// Image watch compares upstream with what each tile runs: the tag now
	// means acme's digest, so only zed's tile is behind.
	_, err = n.orch.images.Checked(ctx, "nginx:1", "sha256:acme", "1", nil)
	must(t, err)
	for tl, behind := range map[string]bool{n.edgeT.ID: false, zt.ID: true} {
		im, err := n.orch.TileImage(ctx, tl)
		must(t, err)
		if im.Newer() != behind {
			t.Errorf("tile %s: runs %s, upstream %s, behind = %v", tl, im.Digest, im.LastDigest, im.Newer())
		}
	}
}

// A registry that refuses an import comes back as one line: the ref once,
// what to do about it, nothing of the registry's own error.
func TestImportDeniedLine(t *testing.T) {
	ctx := context.Background()
	orch, err := New(
		Config{DataDir: t.TempDir(), SecretsKey: testKey, Conntrack: "/nonexistent"},
		WithDocker(dockerfake.New()),
		WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	must(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	orch.regs.WithBase("http://127.0.0.1:1").WithCopier(func(_ context.Context, src, _ string, _ authn.Authenticator, _ http.RoundTripper, _ []string) (imgcopy.Result, error) {
		return imgcopy.Result{}, &imgcopy.DeniedError{Ref: src}
	})
	og := store.Org{ID: "o1", Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}
	must(t, orch.store.Orgs.Create(ctx, og))
	for _, c := range []struct {
		ref  string
		cred store.Credential
		want string
	}{
		{"ghcr.io/acme/x:1", store.Credential{}, "ghcr.io/acme/x:1 is private or missing: connect GitHub for acme or add a ghcr.io credential to the org"},
		{"ghcr.io/acme/x:1", store.Credential{Username: "x-access-token", Password: "t"}, "ghcr.io/acme/x:1 is private or missing: the GitHub connector for acme cannot read it; give its app access to the package or add a ghcr.io credential to the org"},
		{"quay.io/a/b@sha256:abcd", store.Credential{}, "quay.io/a/b@sha256:abcd is private or missing: add a credential for quay.io to the org"},
		{"quay.io/a/b:1", store.Credential{ID: "c1", Username: "u", Password: "p"}, "quay.io/a/b:1 is private or missing: the org's credential for quay.io cannot read it"},
	} {
		_, _, err := orch.importLogged(ctx, og.ID, c.ref, c.cred, []string{"amd64"}, nil)
		if err == nil || err.Error() != c.want {
			t.Errorf("import %s = %v, want %q", c.ref, err, c.want)
		}
	}
}

// ghApp is the GitHub App wrapper for pullCred: every app is installed on
// account and mints "tok-<slug>".
type ghApp struct {
	account string
	mints   int
}

func (g *ghApp) Manifest(string, string, string) (string, string, error) { return "", "", nil }
func (g *ghApp) ConvertManifest(context.Context, string) (githubapp.App, error) {
	return githubapp.App{}, nil
}
func (g *ghApp) Token(ctx context.Context, key string, app githubapp.App) (string, error) {
	tok, _, err := g.Installation(ctx, key, app)
	return tok, err
}
func (g *ghApp) Installation(_ context.Context, _ string, app githubapp.App) (string, string, error) {
	g.mints++
	return "tok-" + app.Slug, g.account, nil
}
func (g *ghApp) Repos(context.Context, string) ([]githubapp.Repo, error) { return nil, nil }
func (g *ghApp) SetHookURL(context.Context, string, githubapp.App) error { return nil }

// pullCred: the org's credential for the host wins; else a ghcr.io ref gets
// the token of the GitHub connector installed on its owner (any case,
// pinned or not); any other host, or an owner no connector is on, is
// anonymous and never asks GitHub.
func TestPullCred(t *testing.T) {
	ctx := context.Background()
	orch, err := New(
		Config{DataDir: t.TempDir(), SecretsKey: testKey, Conntrack: "/nonexistent"},
		WithDocker(dockerfake.New()),
		WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	must(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	gh := &ghApp{account: "FyrmForge"}
	orch.conns = connector.New(orch.store.Connectors, gh)
	og := store.Org{ID: "o1", Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}
	must(t, orch.store.Orgs.Create(ctx, og))
	must(t, orch.store.Connectors.Create(ctx, store.Connector{
		ID: "c1", OrgID: &og.ID, Provider: "github", Name: "gh", Host: "github.com",
		Config: `{"app":{"id":1,"slug":"app","pem":"k"}}`, CreatedAt: time.Now(),
	}))
	for _, c := range []struct{ ref, pass string }{
		{"ghcr.io/fyrmforge/site:latest", "tok-app"},
		{"ghcr.io/FyrmForge/site:1", "tok-app"},
		{"ghcr.io/fyrmforge/site@sha256:abcd", "tok-app"},
		{"ghcr.io/someoneelse/x:1", ""},
		{"quay.io/fyrmforge/site:1", ""},
		{"fyrmforge/site:1", ""},
	} {
		got, ok, err := orch.pullCred(ctx, og.ID, c.ref)
		if err != nil || ok != (c.pass != "") || got.Password != c.pass || ok && (got.Username != "x-access-token" || got.ID != "" || got.URL != "ghcr.io") {
			t.Errorf("pullCred %s = %s %t %v, want password %q", c.ref, got.Username, ok, err, c.pass)
		}
	}
	mints := gh.mints
	if _, _, err := orch.pullCred(ctx, og.ID, "quay.io/fyrmforge/site:1"); err != nil || gh.mints != mints {
		t.Errorf("a non-ghcr ref asked GitHub (%d mints, was %d)", gh.mints, mints)
	}
	cred, err := orch.creds.Create(ctx, og.ID, credential.Spec{Name: "gh pat", URL: "ghcr.io", Username: "me", Password: "pat"})
	must(t, err)
	if got, ok, err := orch.pullCred(ctx, og.ID, "ghcr.io/fyrmforge/site:1"); err != nil || !ok || got.ID != cred.ID || got.Password != "pat" {
		t.Errorf("the org's ghcr.io credential did not win: %s %t %v", got.Username, ok, err)
	}
}
