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
