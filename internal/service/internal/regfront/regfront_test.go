package regfront

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

var key = []byte("test-key")

func TestParseRepo(t *testing.T) {
	if o, s, tl, ok := ParseRepo(Repo("acme", "shop", "web")); !ok || o != "acme" || s != "shop" || tl != "web" {
		t.Fatal("round trip")
	}
	for _, bad := range []string{"", "a/b", "a/b/c/d", "A/b/c", "a//c", "a/b/-c"} {
		if _, _, _, ok := ParseRepo(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDecide(t *testing.T) {
	web, api := "acme/shop/web", "acme/shop/api"
	want := []Scope{{web, []string{Pull, Push}}, {api, []string{Pull}}, {"other/x/y", []string{Pull}}}
	cases := []struct {
		name string
		f    Facts
		want string
	}{
		{"node pulls pinned only", Facts{Kind: KindNode, PullRepos: []string{web}}, "acme/shop/web:pull"},
		{"builder pushes its jobs, push implies pull", Facts{Kind: KindBuilder, PushRepos: []string{web}}, "acme/shop/web:pull,push"},
		{"user has own orgs", Facts{Kind: KindUser, Orgs: []string{"acme"}}, "acme/shop/web:pull,push acme/shop/api:pull"},
		{"user of another org gets nothing", Facts{Kind: KindUser, Orgs: []string{"zed"}}, ""},
		{"admin all", Facts{Kind: KindAdmin}, "acme/shop/web:pull,push acme/shop/api:pull other/x/y:pull"},
		{"no facts no access", Facts{Kind: KindNode}, ""},
	}
	for _, c := range cases {
		var got []string
		for _, s := range Decide(c.f, want) {
			got = append(got, s.String())
		}
		if g := strings.Join(got, " "); g != c.want {
			t.Errorf("%s: got %q want %q", c.name, g, c.want)
		}
	}
	// the catalog is admin only
	cat := []Scope{{CatalogRepo, []string{"*"}}}
	if len(Decide(Facts{Kind: KindUser, Orgs: []string{"acme"}}, cat)) != 0 || len(Decide(Facts{Kind: KindAdmin}, cat)) != 1 {
		t.Error("catalog")
	}
}

func TestParseScopes(t *testing.T) {
	got := ParseScopes([]string{"repository:acme/shop/web:pull,push registry:catalog:*", "bogus", "repository:a/b/c:pull"})
	if len(got) != 3 || got[0].Repo != "acme/shop/web" || len(got[0].Actions) != 2 || got[1].Repo != CatalogRepo || got[2].Repo != "a/b/c" {
		t.Fatalf("%+v", got)
	}
}

func TestTokenVerify(t *testing.T) {
	now := time.Now()
	tok := Sign(key, Claims{Subject: "n1", Expires: now.Add(time.Minute).Unix()})
	if _, err := Verify(key, tok, now); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify([]byte("other"), tok, now); err != ErrToken {
		t.Errorf("wrong key: %v", err)
	}
	if _, err := Verify(key, tok, now.Add(2*time.Minute)); err != ErrExpired {
		t.Errorf("expired: %v", err)
	}
	p, sig, _ := strings.Cut(tok, ".")
	if _, err := Verify(key, p+"x."+sig, now); err != ErrToken {
		t.Errorf("tampered: %v", err)
	}
}

func newServer(t *testing.T, f Facts, audit *[]Issued) (*Server, *httptest.Server, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(be.Close)
	u, _ := url.Parse(be.URL)
	s := &Server{
		Key: key, Realm: "https://registry.example.com", Backend: u,
		Authenticate: func(r *http.Request) (Facts, error) {
			if _, _, ok := r.BasicAuth(); !ok {
				return Facts{}, http.ErrNoCookie
			}
			return f, nil
		},
		Audit: func(_ context.Context, i Issued) error { *audit = append(*audit, i); return nil },
	}
	return s, httptest.NewServer(s), &seen
}

func get(t *testing.T, method, url, bearer string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func mint(t *testing.T, base, scope string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/token?service="+Service+"&scope="+url.QueryEscape(scope), nil)
	req.SetBasicAuth("u", "p")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body.Token == "" {
		t.Fatalf("token body: %v", err)
	}
	return body.Token
}

func TestFrontFlow(t *testing.T) {
	var audit []Issued
	_, front, seen := newServer(t, Facts{Kind: KindNode, Subject: "n1", PullRepos: []string{"acme/shop/web"}}, &audit)
	defer front.Close()

	// the challenge
	res := get(t, "GET", front.URL+"/v2/", "")
	if res.StatusCode != 401 || !strings.Contains(res.Header.Get("WWW-Authenticate"), `realm="https://registry.example.com/token",service="stackr-registry"`) {
		t.Fatalf("challenge: %d %q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}
	// the token endpoint wants credentials
	if res := get(t, "GET", front.URL+"/token", ""); res.StatusCode != 401 {
		t.Fatalf("anonymous token: %d", res.StatusCode)
	}

	tok := mint(t, front.URL, "repository:acme/shop/web:pull,push repository:acme/shop/api:pull")
	if len(audit) != 1 || audit[0].Subject != "n1" || audit[0].Scope != "acme/shop/web:pull" {
		t.Fatalf("audit %+v", audit)
	}

	if res := get(t, "GET", front.URL+"/v2/", tok); res.StatusCode != 200 {
		t.Fatalf("version: %d", res.StatusCode)
	}
	if res := get(t, "GET", front.URL+"/v2/acme/shop/web/manifests/v1", tok); res.StatusCode != 200 {
		t.Fatalf("pull: %d", res.StatusCode)
	}
	if got := (*seen)[len(*seen)-1].Header.Get("Authorization"); got != "" {
		t.Errorf("token leaked to the registry: %q", got)
	}
	// push not granted, other repo not granted, mount from a repo not granted
	for _, c := range []struct{ method, path string }{
		{"PUT", "/v2/acme/shop/web/manifests/v1"},
		{"GET", "/v2/acme/shop/api/manifests/v1"},
		{"POST", "/v2/acme/shop/web/blobs/uploads/?mount=sha256:1&from=acme/shop/api"},
		{"GET", "/v2/_catalog"},
	} {
		res := get(t, c.method, front.URL+c.path, tok)
		if res.StatusCode != 401 || !strings.Contains(res.Header.Get("WWW-Authenticate"), "insufficient_scope") {
			t.Errorf("%s %s: %d", c.method, c.path, res.StatusCode)
		}
	}
	// not a registry path
	if res := get(t, "GET", front.URL+"/v2/acme/shop/web/secret", tok); res.StatusCode != 404 {
		t.Errorf("odd path: %d", res.StatusCode)
	}
	// forged token
	if res := get(t, "GET", front.URL+"/v2/", tok+"x"); res.StatusCode != 401 {
		t.Errorf("forged: %d", res.StatusCode)
	}
}

func TestSystemOrgPullOnly(t *testing.T) {
	repo := SystemOrg + "/tools/rsync"
	got := Decide(Facts{Kind: KindNode, Subject: "n1"}, []Scope{{repo, []string{Pull, Push}}})
	if len(got) != 1 || len(got[0].Actions) != 1 || got[0].Actions[0] != Pull {
		t.Fatalf("node on %s = %v, want pull only", repo, got)
	}
}

// A builder pushing web may not read api: a mount from api becomes a plain
// upload, the registry never sees the mount.
func TestMountWithoutSourceIsPlainUpload(t *testing.T) {
	var audit []Issued
	_, front, seen := newServer(t, Facts{Kind: KindNode, Subject: "n1", PushRepos: []string{"acme/shop/web"}}, &audit)
	defer front.Close()
	tok := mint(t, front.URL, "repository:acme/shop/web:pull,push repository:acme/shop/api:pull")
	res := get(t, "POST", front.URL+"/v2/acme/shop/web/blobs/uploads/?mount=sha256:1&from=acme/shop/api", tok)
	if res.StatusCode == 401 {
		t.Fatalf("mount refused: %d", res.StatusCode)
	}
	if q := (*seen)[len(*seen)-1].URL.RawQuery; q != "" {
		t.Fatalf("registry saw %q, want no mount", q)
	}
}

func TestImportName(t *testing.T) {
	cases := []struct{ ref, repo, tag string }{
		{"postgres:16", "acme/docker.io/library/postgres", "16"},
		{"postgres", "acme/docker.io/library/postgres", "latest"},
		{"library/postgres:16", "acme/docker.io/library/postgres", "16"},
		{"bitnami/redis:7", "acme/docker.io/bitnami/redis", "7"},
		{"docker.io/library/postgres:16", "acme/docker.io/library/postgres", "16"},
		{"index.docker.io/library/postgres:16", "acme/docker.io/library/postgres", "16"},
		{"ghcr.io/foo/bar:1", "acme/ghcr.io/foo/bar", "1"},
		{"ghcr.io/foo/bar/baz/deep:2", "acme/ghcr.io/foo/bar/baz/deep", "2"},
		{"GHCR.io/foo/bar:1", "acme/ghcr.io/foo/bar", "1"},
		{"registry.corp:5443/team/app:3", "acme/registry.corp_5443/team/app", "3"},
		{"ghcr.io/foo/bar@sha256:abc123", "acme/ghcr.io/foo/bar", "sha256-abc123"},
		{"ghcr.io/foo/bar:1@sha256:abc123", "acme/ghcr.io/foo/bar", "sha256-abc123"},
		{"postgres@sha256:abc123", "acme/docker.io/library/postgres", "sha256-abc123"},
	}
	for _, c := range cases {
		repo, tag := ImportName("acme", c.ref)
		if repo != c.repo || tag != c.tag {
			t.Errorf("%s: got %s:%s want %s:%s", c.ref, repo, tag, c.repo, c.tag)
		}
		if ImportRepo("acme", c.ref) != c.repo {
			t.Errorf("%s: ImportRepo differs from ImportName", c.ref)
		}
		if org, up, ok := ParseImportRepo(repo); !ok || org != "acme" || up != strings.TrimPrefix(repo, "acme/") {
			t.Errorf("%s: %s does not parse back: %v %q %q", c.ref, repo, ok, org, up)
		}
	}
}

func TestParseImportRepo(t *testing.T) {
	for _, bad := range []string{"", "a/b", "acme/shop/web", "acme/ghcr.io", "acme/ghcr.io//x", "Acme/ghcr.io/x", "acme/ghcr.io/-x"} {
		if _, _, ok := ParseImportRepo(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSplitPath(t *testing.T) {
	cases := []struct{ path, repo, kind, ref string }{
		{"acme/shop/web/manifests/v1", "acme/shop/web", "manifests", "v1"},
		{"acme/docker.io/library/postgres/manifests/sha256:ab", "acme/docker.io/library/postgres", "manifests", "sha256:ab"},
		{"acme/ghcr.io/a/b/c/d/blobs/sha256:ab", "acme/ghcr.io/a/b/c/d", "blobs", "sha256:ab"},
		{"acme/ghcr.io/a/blobs/uploads", "acme/ghcr.io/a", "blobs", "uploads"},
		{"acme/ghcr.io/a/blobs/uploads/9f1c", "acme/ghcr.io/a", "blobs", "9f1c"},
		{"acme/ghcr.io/a/tags/list", "acme/ghcr.io/a", "tags", ""},
		{"acme/ghcr.io/a/referrers/sha256:ab", "acme/ghcr.io/a", "referrers", "sha256:ab"},
		// a repository part named like an endpoint stays in the repository
		{"acme/ghcr.io/manifests/blobs/sha256:ab", "acme/ghcr.io/manifests", "blobs", "sha256:ab"},
		{"acme/ghcr.io/blobs/uploads/manifests/v1", "acme/ghcr.io/blobs/uploads", "manifests", "v1"},
		{"acme/ghcr.io/manifests/blobs/uploads/9f1c", "acme/ghcr.io/manifests", "blobs", "9f1c"},
	}
	for _, c := range cases {
		repo, kind, ref, ok := splitPath(c.path)
		if !ok || repo != c.repo || kind != c.kind || ref != c.ref {
			t.Errorf("%s: got %q %q %q %v", c.path, repo, kind, ref, ok)
		}
	}
	for _, bad := range []string{"acme/shop/web", "acme/shop/web/secret", "manifests/v1", "a/tags/x"} {
		if repo, _, _, ok := splitPath(bad); ok && validRepo(repo) {
			t.Errorf("%q split as repo %q", bad, repo)
		}
	}
}

func TestDecideImports(t *testing.T) {
	pg := ImportRepo("acme", "postgres:16")
	other := ImportRepo("zed", "postgres:16")
	want := []Scope{{pg, []string{Pull, Push}}, {other, []string{Pull}}}
	cases := []struct {
		name string
		f    Facts
		want string
	}{
		{"user pulls and pushes their org's", Facts{Kind: KindUser, Orgs: []string{"acme"}}, pg + ":pull,push"},
		{"reader pulls only", Facts{Kind: KindUser, PullOrgs: []string{"acme"}}, pg + ":pull"},
		{"admin all", Facts{Kind: KindAdmin}, pg + ":pull,push " + other + ":pull"},
		{"node pulls the import its env uses, not another org's", Facts{Kind: KindNode, PullImports: []string{pg}}, pg + ":pull"},
		{"a node's tile repos do not reach imports", Facts{Kind: KindNode, PullRepos: []string{pg, other}}, ""},
		{"a builder's push list does not either", Facts{Kind: KindBuilder, PushRepos: []string{pg}}, ""},
	}
	for _, c := range cases {
		var got []string
		for _, s := range Decide(c.f, want) {
			got = append(got, s.String())
		}
		if g := strings.Join(got, " "); g != c.want {
			t.Errorf("%s: got %q want %q", c.name, g, c.want)
		}
	}
	sys := SystemOrg + "/docker.io/library/alpine"
	if got := Decide(Facts{Kind: KindNode}, []Scope{{sys, []string{Pull, Push}}}); len(got) != 1 || len(got[0].Actions) != 1 {
		t.Fatalf("system import = %v, want pull only", got)
	}
}

// A node token of org A asking for org B's import is refused at the front,
// whatever scope it asks for.
func TestFrontImportsAcrossOrgs(t *testing.T) {
	var audit []Issued
	a, b := ImportRepo("acme", "ghcr.io/foo/bar:1"), ImportRepo("zed", "ghcr.io/foo/bar:1")
	_, front, _ := newServer(t, Facts{Kind: KindNode, Subject: "n1", PullImports: []string{a}}, &audit)
	defer front.Close()

	tok := mint(t, front.URL, "repository:"+a+":pull repository:"+b+":pull")
	if len(audit) != 1 || audit[0].Scope != a+":pull" {
		t.Fatalf("audit %+v", audit)
	}
	if res := get(t, "GET", front.URL+"/v2/"+a+"/manifests/1", tok); res.StatusCode != 200 {
		t.Fatalf("own import: %d", res.StatusCode)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/v2/" + b + "/manifests/1"},
		{"HEAD", "/v2/" + b + "/blobs/sha256:ab"},
		{"GET", "/v2/" + b + "/tags/list"},
		{"PUT", "/v2/" + a + "/manifests/1"},
		{"POST", "/v2/" + a + "/blobs/uploads/"},
		{"POST", "/v2/" + a + "/blobs/uploads/?mount=sha256:1&from=" + b},
	} {
		res := get(t, c.method, front.URL+c.path, tok)
		if res.StatusCode != 401 || !strings.Contains(res.Header.Get("WWW-Authenticate"), "insufficient_scope") {
			t.Errorf("%s %s: %d", c.method, c.path, res.StatusCode)
		}
	}
}

func TestPushedCallback(t *testing.T) {
	var audit []Issued
	web := "acme/shop/web"
	s, front, _ := newServer(t, Facts{Kind: KindUser, Subject: "u1", Orgs: []string{"acme"}}, &audit)
	defer front.Close()
	var got []Pushed
	s.Pushed = func(_ context.Context, p Pushed) { got = append(got, p) }
	imp := ImportRepo("acme", "ghcr.io/foo/bar:1")
	tok := mint(t, front.URL, "repository:"+web+":pull,push repository:"+imp+":pull,push")

	for _, c := range []struct{ method, path string }{
		{"GET", "/v2/" + web + "/manifests/v1"},
		{"PUT", "/v2/" + web + "/blobs/uploads/9f1c"},
		{"POST", "/v2/" + web + "/blobs/uploads/"},
	} {
		get(t, c.method, front.URL+c.path, tok)
	}
	if len(got) != 0 {
		t.Fatalf("called for %+v", got)
	}
	get(t, "PUT", front.URL+"/v2/"+web+"/manifests/v1", tok)
	get(t, "PUT", front.URL+"/v2/"+imp+"/manifests/1", tok)
	want := []Pushed{
		{Repo: web, Reference: "v1", Kind: KindUser, Subject: "u1"},
		{Repo: imp, Reference: "1", Kind: KindUser, Subject: "u1"},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("pushed = %+v, want %+v", got, want)
	}
}

func TestPushedNotCalledOnFailure(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer be.Close()
	u, _ := url.Parse(be.URL)
	called := false
	s := &Server{
		Key: key, Realm: "https://r", Backend: u,
		Authenticate: func(*http.Request) (Facts, error) { return Facts{Kind: KindAdmin}, nil },
		Pushed:       func(context.Context, Pushed) { called = true },
	}
	tok := Sign(key, Claims{Kind: KindAdmin, Expires: time.Now().Add(time.Minute).Unix(), Access: []Scope{{"acme/shop/web", []string{Push}}}})
	req := httptest.NewRequest("PUT", "/v2/acme/shop/web/manifests/v1", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 400 || called {
		t.Fatalf("code %d, called %v", rec.Code, called)
	}
}
