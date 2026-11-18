// fake-github is a tiny stand-in for GitHub, enough for stackr's GitHub
// connector: the app manifest flow, installation tokens, the repo, branch and
// tree reads, git over https (via git http-backend) and signed webhooks.
// Repos are created bare on first push, any owner and name.
package main

import (
	"bytes"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/cgi"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type app struct {
	ID          int64
	Name        string
	Slug        string
	PEM         string
	Secret      string
	RedirectURL string
	SetupURL    string
	HookURL     string
	Installed   bool
}

type state struct {
	Apps   []*app
	Codes  map[string]int64     // manifest code -> app id, one shot
	Tokens map[string]time.Time // installation token -> expiry
}

type server struct {
	mu    sync.Mutex // guards st and the state file
	st    state
	dir   string
	url   string // public base URL, used in payloads
	git   string
	pushM sync.Mutex // one receive-pack at a time, so before/after snapshots are exact
	hooks sync.WaitGroup
}

func main() {
	listen := flag.String("listen", "0.0.0.0:8443", "listen address")
	dir := flag.String("state", "/var/lib/playground/github", "state directory")
	cert := flag.String("cert", "/var/lib/playground/github/github.pem", "TLS certificate")
	key := flag.String("key", "/var/lib/playground/github/github.key", "TLS key")
	pub := flag.String("url", "https://github.stackr.test:8443", "public base URL")
	flag.Parse()
	s, err := newServer(*dir, *pub)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fake-github on %s", *listen)
	log.Fatal(http.ListenAndServeTLS(*listen, *cert, *key, s.routes()))
}

func newServer(dir, pub string) (*server, error) {
	g, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	s := &server{dir: dir, url: strings.TrimRight(pub, "/"), git: g,
		st: state{Codes: map[string]int64{}, Tokens: map[string]time.Time{}}}
	if err := os.MkdirAll(filepath.Join(dir, "repos"), 0o755); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		err = json.Unmarshal(b, &s.st)
	} else if os.IsNotExist(err) {
		err = nil
	}
	return s, err
}

// save writes state.json; call with mu held.
func (s *server) save() {
	b, _ := json.MarshalIndent(s.st, "", " ")
	if err := os.WriteFile(filepath.Join(s.dir, "state.json"), b, 0o600); err != nil {
		log.Print(err)
	}
}

func (s *server) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /settings/apps/new", s.newApp)
	m.HandleFunc("POST /organizations/{org}/settings/apps/new", s.newApp)
	m.HandleFunc("GET /apps/{slug}/installations/new", s.install)
	m.HandleFunc("POST /api/v3/app-manifests/{code}/conversions", s.convert)
	m.HandleFunc("GET /api/v3/app/installations", s.installations)
	m.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", s.accessToken)
	m.HandleFunc("GET /api/v3/app/hook/config", s.hookConfig)
	m.HandleFunc("PATCH /api/v3/app/hook/config", s.hookConfig)
	m.HandleFunc("GET /api/v3/installation/repositories", s.repositories)
	m.HandleFunc("GET /api/v3/repos/{o}/{r}/branches", s.branches)
	m.HandleFunc("GET /api/v3/repos/{o}/{r}/git/trees/{ref}", s.trees)
	m.HandleFunc("POST /_dev/pr", s.devPR)
	for _, p := range []string{"info/refs", "git-upload-pack", "git-receive-pack"} {
		m.HandleFunc("/{o}/{r}/"+p, s.gitHTTP)
	}
	return m
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// Browser flow.

func (s *server) newApp(w http.ResponseWriter, r *http.Request) {
	var m struct {
		Name        string               `json:"name"`
		RedirectURL string               `json:"redirect_url"`
		SetupURL    string               `json:"setup_url"`
		HookAttrs   struct{ URL string } `json:"hook_attributes"`
	}
	if err := json.Unmarshal([]byte(r.FormValue("manifest")), &m); err != nil || m.Name == "" || m.RedirectURL == "" {
		http.Error(w, "bad manifest", http.StatusUnprocessableEntity)
		return
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	code := randHex(10)
	s.mu.Lock()
	a := &app{ID: int64(len(s.st.Apps)) + 1, Name: m.Name, PEM: pemKey, Secret: randHex(16),
		RedirectURL: m.RedirectURL, SetupURL: m.SetupURL, HookURL: m.HookAttrs.URL}
	a.Slug = strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(m.Name), "-"), "-")
	s.st.Apps = append(s.st.Apps, a)
	s.st.Codes[code] = a.ID
	s.save()
	s.mu.Unlock()
	http.Redirect(w, r, withQuery(m.RedirectURL, "code", code, "state", r.URL.Query().Get("state")), http.StatusFound)
}

// withQuery adds key/value pairs to a URL that may already have a query.
func withQuery(raw string, kv ...string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *server) install(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var a *app
	for _, x := range s.st.Apps {
		if x.Slug == r.PathValue("slug") {
			a = x
		}
	}
	if a != nil {
		a.Installed = true
		s.save()
	}
	s.mu.Unlock()
	if a == nil || a.SetupURL == "" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, withQuery(a.SetupURL, "installation_id", strconv.FormatInt(a.ID, 10),
		"setup_action", "install", "state", r.URL.Query().Get("state")), http.StatusFound)
}

// API.

func (s *server) convert(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.st.Codes[r.PathValue("code")]
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	delete(s.st.Codes, r.PathValue("code"))
	s.save()
	a := s.appByID(id)
	jsonOut(w, 201, map[string]any{"id": a.ID, "slug": a.Slug, "pem": a.PEM, "webhook_secret": a.Secret,
		"name": a.Name, "html_url": s.url + "/apps/" + a.Slug, "client_id": "Iv1." + randHex(8), "client_secret": randHex(20)})
}

func (s *server) appByID(id int64) *app {
	for _, a := range s.st.Apps {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// jwtApp verifies an RS256 app JWT against the key of the app in iss.
func (s *server) jwtApp(r *http.Request) *app {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	p := strings.Split(tok, ".")
	if len(p) != 3 {
		return nil
	}
	var c struct {
		Iss json.RawMessage `json:"iss"`
		Exp int64           `json:"exp"`
	}
	cb, err := base64.RawURLEncoding.DecodeString(p[1])
	if err != nil || json.Unmarshal(cb, &c) != nil || time.Now().Unix() > c.Exp {
		return nil
	}
	id, err := strconv.ParseInt(strings.Trim(string(c.Iss), `"`), 10, 64)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	var a *app
	if x := s.appByID(id); x != nil {
		cp := *x // a snapshot: callers read it after the lock is gone
		a = &cp
	}
	s.mu.Unlock()
	if a == nil {
		return nil
	}
	block, _ := pem.Decode([]byte(a.PEM))
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	sig, err2 := base64.RawURLEncoding.DecodeString(p[2])
	if err != nil || err2 != nil {
		return nil
	}
	h := sha256.Sum256([]byte(p[0] + "." + p[1]))
	if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, h[:], sig) != nil {
		return nil
	}
	return a
}

func (s *server) installations(w http.ResponseWriter, r *http.Request) {
	a := s.jwtApp(r)
	if a == nil {
		http.Error(w, "bad credentials", 401)
		return
	}
	out := []map[string]any{}
	if a.Installed {
		out = append(out, map[string]any{"id": a.ID})
	}
	jsonOut(w, 200, out)
}

func (s *server) accessToken(w http.ResponseWriter, r *http.Request) {
	a := s.jwtApp(r)
	if a == nil {
		http.Error(w, "bad credentials", 401)
		return
	}
	if !a.Installed || r.PathValue("id") != strconv.FormatInt(a.ID, 10) {
		http.NotFound(w, r)
		return
	}
	tok, exp := "ghs_"+randHex(20), time.Now().Add(time.Hour).UTC()
	s.mu.Lock()
	s.st.Tokens[tok] = exp
	s.save()
	s.mu.Unlock()
	jsonOut(w, 201, map[string]any{"token": tok, "expires_at": exp.Format(time.RFC3339)})
}

func (s *server) hookConfig(w http.ResponseWriter, r *http.Request) {
	a := s.jwtApp(r)
	if a == nil {
		http.Error(w, "bad credentials", 401)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a = s.appByID(a.ID) // the live app, a is a snapshot
	if r.Method == http.MethodPatch {
		var b struct {
			URL *string `json:"url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.URL != nil {
			a.HookURL = *b.URL
			s.save()
		}
	}
	jsonOut(w, 200, map[string]any{"url": a.HookURL, "content_type": "json"})
}

// tokenOK reports whether tok is a live installation token.
func (s *server) tokenOK(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.st.Tokens[tok]
	return ok && time.Now().Before(exp)
}

func (s *server) bearerOK(w http.ResponseWriter, r *http.Request) bool {
	if s.tokenOK(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
		return true
	}
	http.Error(w, "bad credentials", 401)
	return false
}

func (s *server) repoDir(o, n string) string {
	return filepath.Join(s.dir, "repos", filepath.Base(o), strings.TrimSuffix(filepath.Base(n), ".git")+".git")
}

func (s *server) run(dir string, args ...string) (string, error) {
	cmd := exec.Command(s.git, args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

func (s *server) defaultBranch(dir string) string {
	b, err := s.run(dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "main"
	}
	return b
}

// repoInfo is the repository object of a payload or listing.
func (s *server) repoInfo(o, n string) map[string]any {
	full := o + "/" + n
	return map[string]any{"full_name": full, "name": n, "private": true,
		"default_branch": s.defaultBranch(s.repoDir(o, n)),
		"clone_url":      s.url + "/" + full, "ssh_url": "git@" + hostOf(s.url) + ":" + full + ".git",
		"html_url": s.url + "/" + full, "owner": map[string]any{"login": o}}
}

func hostOf(raw string) string {
	u, _ := url.Parse(raw)
	return u.Hostname()
}

func (s *server) repositories(w http.ResponseWriter, r *http.Request) {
	if !s.bearerOK(w, r) {
		return
	}
	dirs, _ := filepath.Glob(filepath.Join(s.dir, "repos", "*", "*.git"))
	all := []map[string]any{}
	for _, d := range dirs {
		all = append(all, s.repoInfo(filepath.Base(filepath.Dir(d)), strings.TrimSuffix(filepath.Base(d), ".git")))
	}
	// ponytail: honors per_page/page only enough to end the connector's loop
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if per <= 0 {
		per = 30
	}
	lo := min((max(page, 1)-1)*per, len(all))
	jsonOut(w, 200, map[string]any{"total_count": len(all), "repositories": all[lo:min(lo+per, len(all))]})
}

func (s *server) existingRepo(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !s.bearerOK(w, r) {
		return "", false
	}
	d := s.repoDir(r.PathValue("o"), r.PathValue("r"))
	if _, err := os.Stat(d); err != nil {
		http.NotFound(w, r)
		return "", false
	}
	return d, true
}

func (s *server) branches(w http.ResponseWriter, r *http.Request) {
	d, ok := s.existingRepo(w, r)
	if !ok {
		return
	}
	out := []map[string]any{}
	for _, l := range s.refs(d) {
		out = append(out, map[string]any{"name": strings.TrimPrefix(l[1], "refs/heads/"), "commit": map[string]any{"sha": l[0]}})
	}
	if pg, _ := strconv.Atoi(r.URL.Query().Get("page")); pg > 1 {
		out = out[:0]
	}
	jsonOut(w, 200, out)
}

// refs lists [sha, refname] of every branch.
func (s *server) refs(dir string) [][2]string {
	out, _ := s.run(dir, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads")
	var res [][2]string
	for _, l := range strings.Split(out, "\n") {
		if sha, name, ok := strings.Cut(l, " "); ok {
			res = append(res, [2]string{sha, name})
		}
	}
	return res
}

func (s *server) trees(w http.ResponseWriter, r *http.Request) {
	d, ok := s.existingRepo(w, r)
	if !ok {
		return
	}
	ref := r.PathValue("ref")
	sha, err := s.run(d, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	out, _ := s.run(d, "-c", "core.quotepath=off", "ls-tree", "-r", "-t", ref)
	tree := []map[string]any{}
	for _, l := range strings.Split(out, "\n") {
		meta, path, ok := strings.Cut(l, "\t")
		if f := strings.Fields(meta); ok && len(f) == 3 {
			tree = append(tree, map[string]any{"path": path, "type": f[1], "sha": f[2]})
		}
	}
	jsonOut(w, 200, map[string]any{"sha": sha, "tree": tree, "truncated": false})
}

// Git over https.

func (s *server) gitHTTP(w http.ResponseWriter, r *http.Request) {
	u, p, ok := r.BasicAuth()
	if !ok || !(u == "dev" || (u == "x-access-token" && s.tokenOK(p))) {
		w.Header().Set("WWW-Authenticate", `Basic realm="fake-github"`)
		http.Error(w, "authentication required", 401)
		return
	}
	o, n := r.PathValue("o"), r.PathValue("r")
	d := s.repoDir(o, n)
	if _, err := os.Stat(d); err != nil && strings.HasSuffix(r.URL.Path, "info/refs") && r.URL.Query().Get("service") == "git-receive-pack" {
		err := os.MkdirAll(d, 0o755)
		if err == nil {
			_, err = s.run(d, "init", "--bare", "-b", "main")
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	rel := strings.TrimSuffix(n, ".git") + ".git"
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + o + "/" + rel + "/" + strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"+o+"/"+n+"/"), "?", 2)[0]
	h := &cgi.Handler{Path: s.git, Args: []string{"http-backend"}, InheritEnv: []string{"PATH", "HOME"},
		Env: []string{"GIT_PROJECT_ROOT=" + filepath.Join(s.dir, "repos"), "GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true"}}
	if !strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
		h.ServeHTTP(w, r2)
		return
	}
	s.pushM.Lock()
	defer s.pushM.Unlock()
	before := s.refs(d)
	h.ServeHTTP(w, r2)
	s.hooks.Add(1)
	go func(after [][2]string) {
		defer s.hooks.Done()
		s.pushEvents(o, strings.TrimSuffix(n, ".git"), before, after)
	}(s.refs(d))
}

// Webhooks.

const zero = "0000000000000000000000000000000000000000"

func (s *server) pushEvents(o, n string, before, after [][2]string) {
	d := s.repoDir(o, n)
	old := map[string]string{}
	for _, b := range before {
		old[b[1]] = b[0]
	}
	seen := map[string]bool{}
	for _, a := range after {
		seen[a[1]] = true
		if old[a[1]] != a[0] {
			s.deliver("push", s.pushBody(d, o, n, a[1], old[a[1]], a[0]))
		}
	}
	for _, b := range before {
		if !seen[b[1]] {
			s.deliver("push", s.pushBody(d, o, n, b[1], b[0], zero))
		}
	}
}

func (s *server) pushBody(d, o, n, ref, before, after string) map[string]any {
	if before == "" {
		before = zero
	}
	b := map[string]any{"ref": ref, "before": before, "after": after, "created": before == zero,
		"deleted": after == zero, "commits": []any{}, "repository": s.repoInfo(o, n)}
	if after == zero {
		return b
	}
	msg, _ := s.run(d, "log", "-1", "--format=%B", after)
	b["head_commit"] = map[string]any{"id": after, "message": msg}
	if before != zero {
		c := map[string]any{"id": after, "message": msg, "added": []string{}, "removed": []string{}, "modified": []string{}}
		out, _ := s.run(d, "-c", "core.quotepath=off", "diff", "--name-status", before, after)
		for _, l := range strings.Split(out, "\n") {
			st, path, ok := strings.Cut(l, "\t")
			if !ok {
				continue
			}
			k := map[string]string{"A": "added", "D": "removed"}[st[:1]]
			if k == "" {
				k = "modified" // M, R, C, T: a path to look at
			}
			c[k] = append(c[k].([]string), path[strings.LastIndex(path, "\t")+1:])
		}
		b["commits"] = []any{c}
	}
	return b
}

var insecure = &http.Client{Timeout: 10 * time.Second,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

// deliver posts one event to every installed app's hook URL, signed per app.
func (s *server) deliver(event string, payload any) {
	body, _ := json.Marshal(payload)
	s.mu.Lock()
	apps := make([]app, len(s.st.Apps))
	for i, a := range s.st.Apps {
		apps[i] = *a
	}
	s.mu.Unlock()
	for _, a := range apps {
		if !a.Installed || a.HookURL == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(a.Secret))
		mac.Write(body)
		req, err := http.NewRequest(http.MethodPost, a.HookURL, bytes.NewReader(body))
		if err != nil {
			log.Print(err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-GitHub-Delivery", randHex(16))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		res, err := insecure.Do(req)
		if err != nil {
			log.Printf("hook %s: %v", a.HookURL, err)
			continue
		}
		log.Printf("hook %s %s: %s", event, a.HookURL, res.Status)
		_ = res.Body.Close()
	}
}

// devPR sends a pull_request event. fork=1 puts the head in another owner's repo.
func (s *server) devPR(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	o, n, ok := strings.Cut(q.Get("repo"), "/")
	num, err := strconv.Atoi(q.Get("number"))
	if !ok || err != nil || q.Get("action") == "" || q.Get("head") == "" {
		http.Error(w, "need repo=o/r&number=N&action=...&head=B", 400)
		return
	}
	base := q.Get("base")
	if base == "" {
		base = "main"
	}
	d := s.repoDir(o, n)
	sha, err := s.run(d, "rev-parse", "--verify", "refs/heads/"+q.Get("head"))
	if err != nil {
		http.Error(w, "repo or head branch not found", http.StatusNotFound)
		return
	}
	headRepo := o + "/" + n
	if q.Get("fork") == "1" {
		headRepo = "fork-" + o + "/" + n
	}
	s.deliver("pull_request", map[string]any{"action": q.Get("action"), "number": num,
		"pull_request": map[string]any{"number": num, "merged": false,
			"head": map[string]any{"ref": q.Get("head"), "sha": sha, "repo": map[string]any{"full_name": headRepo}},
			"base": map[string]any{"ref": base}},
		"repository": s.repoInfo(o, n)})
	w.WriteHeader(http.StatusAccepted)
}
