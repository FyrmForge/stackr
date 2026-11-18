package main

import (
	"bytes"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type delivery struct {
	event, sig string
	body       []byte
}

func do(t *testing.T, c *http.Client, method, u, bearer string, body io.Reader) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, u, body)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func jwt(t *testing.T, id, pemKey string) string {
	block, _ := pem.Decode([]byte(pemKey))
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	now := time.Now().Unix()
	un := enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." + enc(map[string]any{"iat": now - 60, "exp": now + 540, "iss": id})
	h := sha256.Sum256([]byte(un))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	return un + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func git(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null")...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestFlow(t *testing.T) {
	s, err := newServer(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewTLSServer(s.routes())
	defer ts.Close()
	s.url = ts.URL
	c := ts.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	hooks := make(chan delivery, 8)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hooks <- delivery{r.Header.Get("X-GitHub-Event"), r.Header.Get("X-Hub-Signature-256"), b}
	}))
	defer hs.Close()

	// manifest -> redirect with code -> conversion
	manifest, _ := json.Marshal(map[string]any{"name": "stackr-Panel.test-ab12", "redirect_url": "https://panel/cb",
		"setup_url": "https://panel/ret", "hook_attributes": map[string]string{"url": "https://old/hook"}})
	res, err := c.PostForm(ts.URL+"/settings/apps/new?state=S1", url.Values{"manifest": {string(manifest)}})
	if err != nil || res.StatusCode != 302 {
		t.Fatalf("manifest: %v %v", err, res)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	if loc.Host != "panel" || loc.Query().Get("state") != "S1" || loc.Query().Get("code") == "" {
		t.Fatalf("redirect %s", loc)
	}
	code := loc.Query().Get("code")
	st, b := do(t, c, "POST", ts.URL+"/api/v3/app-manifests/"+code+"/conversions", "", nil)
	var app struct {
		ID            int64
		Slug, PEM     string
		WebhookSecret string `json:"webhook_secret"`
	}
	if st != 201 || json.Unmarshal(b, &app) != nil || app.Slug != "stackr-panel-test-ab12" || app.PEM == "" {
		t.Fatalf("conversion %d %s", st, b)
	}
	if st, _ = do(t, c, "POST", ts.URL+"/api/v3/app-manifests/"+code+"/conversions", "", nil); st != 404 {
		t.Fatalf("code reused: %d", st)
	}

	// not installed yet, then installed
	j := jwt(t, fmt.Sprint(app.ID), app.PEM)
	if _, b = do(t, c, "GET", ts.URL+"/api/v3/app/installations", j, nil); string(bytes.TrimSpace(b)) != "[]" {
		t.Fatalf("installations before install: %s", b)
	}
	if st, _ = do(t, c, "GET", ts.URL+"/api/v3/app/installations", "garbage", nil); st != 401 {
		t.Fatalf("bad jwt: %d", st)
	}
	res, _ = c.Get(ts.URL + "/apps/" + app.Slug + "/installations/new?state=S2")
	loc, _ = url.Parse(res.Header.Get("Location"))
	if res.StatusCode != 302 || loc.Path != "/ret" || loc.Query().Get("installation_id") == "" || loc.Query().Get("state") != "S2" {
		t.Fatalf("install redirect %v", loc)
	}
	_, b = do(t, c, "GET", ts.URL+"/api/v3/app/installations", j, nil)
	var insts []struct{ ID int64 }
	if json.Unmarshal(b, &insts) != nil || len(insts) != 1 {
		t.Fatalf("installations: %s", b)
	}
	if st, _ = do(t, c, "PATCH", ts.URL+"/api/v3/app/hook/config", j, strings.NewReader(`{"url":"`+hs.URL+`"}`)); st != 200 {
		t.Fatalf("hook config: %d", st)
	}
	_, b = do(t, c, "GET", ts.URL+"/api/v3/app/hook/config", j, nil)
	if !strings.Contains(string(b), hs.URL) {
		t.Fatalf("hook url not stored: %s", b)
	}
	st, b = do(t, c, "POST", fmt.Sprintf("%s/api/v3/app/installations/%d/access_tokens", ts.URL, insts[0].ID), j, nil)
	var tok struct {
		Token     string
		ExpiresAt time.Time `json:"expires_at"`
	}
	if st != 201 || json.Unmarshal(b, &tok) != nil || tok.Token == "" || time.Until(tok.ExpiresAt) < 50*time.Minute {
		t.Fatalf("token %d %s", st, b)
	}

	// real git push, no token: 401; with token: ok
	ca := filepath.Join(t.TempDir(), "ca.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o600)
	work := t.TempDir()
	git(t, work, nil, "init", "-b", "main")
	git(t, work, nil, "config", "user.email", "a@b.c")
	git(t, work, nil, "config", "user.name", "a")
	_ = os.WriteFile(filepath.Join(work, "stackr.yaml"), []byte("a: 1\n"), 0o644)
	git(t, work, nil, "add", ".")
	git(t, work, nil, "commit", "-m", "first")
	cfg := []string{"-c", "http.sslCAInfo=" + ca}
	if out, err := exec.Command("git", append(cfg, "-C", work, "push", "https://"+strings.TrimPrefix(ts.URL, "https://")+"/acme/app.git", "main")...).CombinedOutput(); err == nil {
		t.Fatalf("push without auth worked: %s", out)
	}
	pushURL := "https://x-access-token:" + tok.Token + "@" + strings.TrimPrefix(ts.URL, "https://") + "/acme/app"
	git(t, work, nil, append(cfg, "push", pushURL, "main")...)
	h1 := <-hooks
	checkHMAC(t, h1, app.WebhookSecret)
	var m1 map[string]any
	_ = json.Unmarshal(h1.body, &m1)
	repo := m1["repository"].(map[string]any)
	sha := git(t, work, nil, "rev-parse", "HEAD")
	if h1.event != "push" || m1["ref"] != "refs/heads/main" || m1["after"] != sha || m1["created"] != true ||
		repo["clone_url"] != ts.URL+"/acme/app" || repo["full_name"] != "acme/app" || repo["default_branch"] != "main" ||
		repo["owner"].(map[string]any)["login"] != "acme" || m1["head_commit"].(map[string]any)["message"] != "first" ||
		len(m1["commits"].([]any)) != 0 {
		t.Fatalf("first push payload: %s", h1.body)
	}

	// second push: commit list with file changes
	_ = os.WriteFile(filepath.Join(work, "b.yml"), []byte("b: 1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(work, "stackr.yaml"), []byte("a: 2\n"), 0o644)
	git(t, work, nil, "add", ".")
	git(t, work, nil, "commit", "-m", "second")
	git(t, work, nil, append(cfg, "push", pushURL, "main")...)
	h2 := <-hooks
	checkHMAC(t, h2, app.WebhookSecret)
	var m2 struct {
		Commits []struct{ Added, Removed, Modified []string }
	}
	_ = json.Unmarshal(h2.body, &m2)
	if len(m2.Commits) != 1 || fmt.Sprint(m2.Commits[0].Added) != "[b.yml]" || fmt.Sprint(m2.Commits[0].Modified) != "[stackr.yaml]" {
		t.Fatalf("second push payload: %s", h2.body)
	}

	// clone works with the connector's extraheader style auth
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok.Token))
	git(t, t.TempDir(), nil, append(cfg, "-c", "http.extraheader=Authorization: Basic "+basic, "ls-remote", "--symref", ts.URL+"/acme/app", "HEAD")...)

	// REST reads
	_, b = do(t, c, "GET", ts.URL+"/api/v3/installation/repositories?per_page=100&page=1", tok.Token, nil)
	if !strings.Contains(string(b), `"full_name":"acme/app"`) || !strings.Contains(string(b), `"private":true`) || !strings.Contains(string(b), `"total_count":1`) {
		t.Fatalf("repositories: %s", b)
	}
	if st, _ = do(t, c, "GET", ts.URL+"/api/v3/installation/repositories", "nope", nil); st != 401 {
		t.Fatalf("repos without token: %d", st)
	}
	_, b = do(t, c, "GET", ts.URL+"/api/v3/repos/acme/app/branches?per_page=100&page=1", tok.Token, nil)
	if !strings.Contains(string(b), `"name":"main"`) || !strings.Contains(string(b), git(t, work, nil, "rev-parse", "HEAD")) {
		t.Fatalf("branches: %s", b)
	}
	_, b = do(t, c, "GET", ts.URL+"/api/v3/repos/acme/app/git/trees/main?recursive=1", tok.Token, nil)
	if !strings.Contains(string(b), `"path":"b.yml"`) || !strings.Contains(string(b), `"type":"blob"`) {
		t.Fatalf("tree: %s", b)
	}

	// dev PR event, plain and fork
	if st, b = do(t, c, "POST", ts.URL+"/_dev/pr?repo=acme/app&number=7&action=opened&head=main", "", nil); st != 202 {
		t.Fatalf("dev pr: %d %s", st, b)
	}
	h3 := <-hooks
	checkHMAC(t, h3, app.WebhookSecret)
	var pr struct {
		Action string
		Number int
		PR     struct {
			Head struct {
				Ref, SHA string
				Repo     struct {
					FullName string `json:"full_name"`
				}
			}
			Base struct{ Ref string }
		} `json:"pull_request"`
	}
	_ = json.Unmarshal(h3.body, &pr)
	if h3.event != "pull_request" || pr.Action != "opened" || pr.Number != 7 || pr.PR.Head.Ref != "main" ||
		pr.PR.Head.SHA != git(t, work, nil, "rev-parse", "HEAD") || pr.PR.Head.Repo.FullName != "acme/app" || pr.PR.Base.Ref != "main" {
		t.Fatalf("pr payload: %s", h3.body)
	}
	do(t, c, "POST", ts.URL+"/_dev/pr?repo=acme/app&number=8&action=opened&head=main&fork=1", "", nil)
	if h4 := <-hooks; strings.Contains(string(h4.body), `"full_name":"acme/app"},"base"`) {
		t.Fatalf("fork head repo not different: %s", h4.body)
	}
	for _, u := range []string{"repo=acme/app&head=nope", "repo=acme/missing&head=main"} {
		if st, _ := do(t, c, "POST", ts.URL+"/_dev/pr?"+u+"&number=9&action=opened", "", nil); st != 404 {
			t.Fatalf("dev pr %s: %d, want 404", u, st)
		}
	}
	s.hooks.Wait()
}

func checkHMAC(t *testing.T, d delivery, secret string) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(d.body)
	if d.sig != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("bad signature on %s", d.event)
	}
}
