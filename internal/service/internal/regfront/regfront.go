// Package regfront fronts the head's local registry. The registry itself
// (installspec.Registry) serves a unix socket and checks nothing; this package
// speaks Docker's token auth to clients and enforces the token on every call
// it proxies.
//
// Image naming has two shapes. A tile's image is one repository,
// "<org>/<stack>/<tile>", each part a slug (lower-case letters, digits, '.',
// '_', '-'). Workers pull "localhost:5000/<org>/<stack>/<tile>:<tag>",
// developers "registry.<panel domain>/<org>/<stack>/<tile>:<tag>". The tag is
// the caller's (a build id); nothing here reads it.
//
// An import is a copy of an outside image: the upstream name with the org in
// front, "<org>/<host>/<path...>" ("acme/docker.io/library/postgres"), any
// depth. The second part always holds a dot (a registry host) and a stack
// slug never does, so the two shapes cannot be confused. A request path
// splits into repository and endpoint from its end, as the registry itself
// routes it: "/manifests/<ref>", "/blobs/<digest>", "/blobs/uploads/[<id>]",
// "/tags/list", "/referrers/<digest>".
//
// Who may do what is decided from facts the caller passes in (Facts): the
// caller knows which envs are pinned where and which build jobs run on which
// builder. This package knows no store.
//
// Tokens are opaque to Docker clients: "<claims>.<hmac>", signed with the
// caller's key, short lived, listing the granted access. Every issue is
// reported through Server.Audit.
package regfront

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Kind is who a token is for; it is the audit record's kind.
type Kind string

const (
	KindNode    Kind = "node"
	KindBuilder Kind = "builder"
	KindUser    Kind = "user"
	KindAdmin   Kind = "admin"
)

const (
	Pull = "pull"
	Push = "push"
	// CatalogRepo is the pseudo repository the registry catalog needs;
	// only admin is ever granted it.
	CatalogRepo = "registry:catalog"
	// Service is the "service" value in the challenge.
	Service = "stackr-registry"
	// DefaultTTL is how long a token lives.
	DefaultTTL = 5 * time.Minute
)

// SystemOrg holds stackr's own images (the live-move rsync helper). The dot
// keeps it out of the org slug space; every authenticated caller may pull it
// and none may push it: the controller publishes straight to the registry.
const SystemOrg = "stackr.system"

var slug = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// Repo is the repository of a tile's image.
func Repo(org, stack, tile string) string { return org + "/" + stack + "/" + tile }

// ParseRepo splits a repository into org, stack and tile slugs.
func ParseRepo(repo string) (org, stack, tile string, ok bool) {
	p := strings.Split(repo, "/")
	if len(p) != 3 {
		return "", "", "", false
	}
	for _, s := range p {
		if !slug.MatchString(s) {
			return "", "", "", false
		}
	}
	return p[0], p[1], p[2], true
}

// ImportRepo is the repository an outside image is copied to: the org in
// front of the upstream name, nothing invented. Docker Hub short names are
// normalised ("postgres:16" and "library/postgres" both become
// "<org>/docker.io/library/postgres"). The tag or digest is not part of it
// (see ImportName). A host port cannot sit in a repository name, so
// "host:5443" becomes "host_5443".
func ImportRepo(org, upstreamRef string) string {
	repo, _ := ImportName(org, upstreamRef)
	return repo
}

// ImportName is ImportRepo plus the tag the copy lands under: the upstream
// tag, "latest" when the ref has none, or "sha256-<hex>" for a digest pinned
// ref, so a tag always names a version and nothing the registry cleans as
// untagged can be a running image.
func ImportName(org, upstreamRef string) (repo, tag string) {
	ref := strings.TrimSpace(upstreamRef)
	tag = "latest"
	if i := strings.Index(ref, "@"); i >= 0 {
		tag = strings.Replace(ref[i+1:], ":", "-", 1)
		ref = ref[:i]
		if j := strings.LastIndex(ref, ":"); j > strings.LastIndex(ref, "/") {
			ref = ref[:j] // name:tag@digest: the digest wins
		}
	} else if j := strings.LastIndex(ref, ":"); j > strings.LastIndex(ref, "/") {
		tag, ref = ref[j+1:], ref[:j]
	}
	host, path, hasHost := strings.Cut(ref, "/")
	if !hasHost || (!strings.ContainsAny(host, ".:") && host != "localhost") {
		host, path = "docker.io", ref // a Docker Hub short name
	}
	host = strings.ToLower(strings.ReplaceAll(host, ":", "_"))
	switch host {
	case "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		host = "docker.io"
	}
	if host == "docker.io" && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	return org + "/" + host + "/" + path, tag
}

// systemRepo reports a repo under SystemOrg, of any depth
// (stackr.system/stackr, stackr.system/tools/rsync).
func systemRepo(repo string) bool {
	p := strings.Split(repo, "/")
	if len(p) < 2 || p[0] != SystemOrg {
		return false
	}
	for _, s := range p[1:] {
		if !slug.MatchString(s) {
			return false
		}
	}
	return true
}

// ParseImportRepo splits an import repository into the org and the upstream
// name ("docker.io/library/postgres"). Any depth of three parts or more, every
// part a slug, the second one a host (it holds a dot).
func ParseImportRepo(repo string) (org, upstream string, ok bool) {
	p := strings.Split(repo, "/")
	if len(p) < 3 || !strings.Contains(p[1], ".") {
		return "", "", false
	}
	for _, s := range p {
		if !slug.MatchString(s) {
			return "", "", false
		}
	}
	return p[0], strings.Join(p[1:], "/"), true
}

// Scope is access to one repository: actions are pull and push.
type Scope struct {
	Repo    string   `json:"repo"`
	Actions []string `json:"actions"`
}

func (s Scope) String() string { return s.Repo + ":" + strings.Join(s.Actions, ",") }

// Facts is what the caller knows about the authenticated principal.
type Facts struct {
	Kind    Kind
	Subject string // node, user or key id, for the audit
	// Orgs are the org slugs a user may pull and push; PullOrgs only pull.
	Orgs     []string
	PullOrgs []string
	// PullRepos are the repos of envs pinned to the node.
	PullRepos []string
	// PullImports are the import repos (see ImportRepo) of the upstream refs
	// that envs pinned to the node run. A node pulls an import only from
	// here, never by org: one node hosts many orgs.
	PullImports []string
	// PushRepos are the repos of the builder's build jobs; pushing implies
	// pulling them.
	PushRepos []string
}

func (f Facts) allowed(repo, action string) bool {
	// Before the admin shortcut: nodes run these images as root, so nothing
	// writes them through the front, admins included.
	if systemRepo(repo) {
		return action == Pull
	}
	if f.Kind == KindAdmin {
		return true
	}
	org, _, _, ok := ParseRepo(repo)
	imp := false
	if o, _, isImp := ParseImportRepo(repo); isImp {
		org, ok, imp = o, true, true
	}
	if !ok {
		return false
	}
	if org == SystemOrg {
		return action == Pull
	}
	if slices.Contains(f.Orgs, org) {
		return action == Pull || action == Push
	}
	if action == Pull && slices.Contains(f.PullOrgs, org) {
		return true
	}
	if imp { // nodes: exactly the imports their envs run
		return action == Pull && slices.Contains(f.PullImports, repo)
	}
	switch action {
	case Pull:
		return slices.Contains(f.PullRepos, repo) || slices.Contains(f.PushRepos, repo)
	case Push:
		return slices.Contains(f.PushRepos, repo)
	}
	return false
}

// Decide is the part of want that f may do. A repo with nothing allowed is
// left out, as Docker's token auth does.
func Decide(f Facts, want []Scope) []Scope {
	var out []Scope
	for _, w := range want {
		var acts []string
		for _, a := range w.Actions {
			if f.allowed(w.Repo, a) && !slices.Contains(acts, a) {
				acts = append(acts, a)
			}
		}
		if len(acts) > 0 {
			out = append(out, Scope{w.Repo, acts})
		}
	}
	return out
}

// ParseScopes reads Docker's scope parameters ("repository:<repo>:pull,push",
// "registry:catalog:*"). Unknown types are dropped.
func ParseScopes(params []string) []Scope {
	var out []Scope
	for _, p := range params {
		for _, one := range strings.Fields(p) {
			typ, rest, ok := strings.Cut(one, ":")
			i := strings.LastIndex(rest, ":")
			if !ok || i < 0 {
				continue
			}
			name, acts := rest[:i], strings.Split(rest[i+1:], ",")
			switch {
			case typ == "repository":
				out = append(out, Scope{name, acts})
			case typ == "registry" && name == "catalog":
				out = append(out, Scope{CatalogRepo, acts})
			}
		}
	}
	return out
}

// Claims is what a token says.
type Claims struct {
	ID      string  `json:"jti"`
	Kind    Kind    `json:"kind"`
	Subject string  `json:"sub"`
	Issued  int64   `json:"iat"`
	Expires int64   `json:"exp"`
	Access  []Scope `json:"access"`
}

func (c Claims) permits(repo, action string) bool {
	for _, s := range c.Access {
		if s.Repo == repo && (slices.Contains(s.Actions, action) || slices.Contains(s.Actions, "*")) {
			return true
		}
	}
	return false
}

var (
	ErrToken   = errors.New("regfront: bad token")
	ErrExpired = errors.New("regfront: token expired")
)

func sign(key []byte, payload string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Sign makes the token for c.
func Sign(key []byte, c Claims) string {
	b, _ := json.Marshal(c) // plain fields: cannot fail
	p := base64.RawURLEncoding.EncodeToString(b)
	return p + "." + sign(key, p)
}

// Verify checks the signature and expiry.
func Verify(key []byte, token string, now time.Time) (Claims, error) {
	var c Claims
	p, sig, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(sign(key, p))) {
		return c, ErrToken
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil || json.Unmarshal(b, &c) != nil {
		return c, ErrToken
	}
	if now.Unix() >= c.Expires {
		return c, ErrExpired
	}
	return c, nil
}

// Issued is one audit record.
type Issued struct {
	ID        string
	Kind      Kind
	Subject   string
	Scope     string // granted, space-separated Scope strings
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Pushed is one finished manifest push.
type Pushed struct {
	Repo      string
	Reference string // the tag, or the digest when pushed by digest
	Digest    string // the registry's Docker-Content-Digest for it
	Kind      Kind
	Subject   string
}

// Gate shuts the registry's writes for a while: the cleanup sweep closes it
// around the garbage collect, the front answers 503 while it is shut and an
// import (which writes to the registry directly) waits on it. The zero Gate is
// open.
type Gate struct {
	mu      sync.Mutex
	shut    chan struct{} // non-nil while closed; closed when the gate opens
	writers int           // writes in flight (TryEnter/Enter not yet left)
}

// TryEnter starts a write when the gate is open; leave ends it. ok is false
// while the gate is shut. Drain waits for every entered write to leave.
func (g *Gate) TryEnter() (leave func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut != nil {
		return nil, false
	}
	g.writers++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.writers--
			g.mu.Unlock()
		})
	}, true
}

// Enter waits for the gate to open, then starts a write as TryEnter does. A
// writer holds it for its whole write (an import's every blob and manifest),
// so a sweep never collects blobs a write has put but not yet tagged.
func (g *Gate) Enter(ctx context.Context) (leave func(), err error) {
	for {
		if leave, ok := g.TryEnter(); ok {
			return leave, nil
		}
		if err := g.Wait(ctx); err != nil {
			return nil, err
		}
	}
}

// Drain waits until no write is in flight, or ctx's error. Call it with the
// gate shut, so no new write starts meanwhile.
// ponytail: polls every 50 ms; a sweep runs nightly.
func (g *Gate) Drain(ctx context.Context) error {
	for {
		g.mu.Lock()
		n := g.writers
		g.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Close shuts the gate and returns the func that opens it again; ok is false
// when it was already shut (another sweep), and nothing was changed.
func (g *Gate) Close() (open func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut != nil {
		return nil, false
	}
	shut := make(chan struct{})
	g.shut = shut
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.shut == shut {
			g.shut = nil
			close(shut)
		}
	}, true
}

// Closed says whether the gate is shut.
func (g *Gate) Closed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.shut != nil
}

// Wait returns once the gate is open, or ctx's error.
func (g *Gate) Wait(ctx context.Context) error {
	for {
		g.mu.Lock()
		shut := g.shut
		g.mu.Unlock()
		if shut == nil {
			return nil
		}
		select {
		case <-shut:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// gateRetryAfter is the seconds a refused writer is told to wait. The sweep
// takes seconds; a builder retries on 503.
const gateRetryAfter = "10"

// Server is the front: the token endpoint at /token and the proxy under /v2/.
type Server struct {
	Key []byte
	// Realm is the front's public base URL, where /token is.
	Realm   string
	Backend *url.URL // the local registry
	// Transport reaches Backend (the registry's unix socket); nil = TCP.
	Transport http.RoundTripper
	TTL       time.Duration
	// Authenticate reads the request's credentials (Basic auth, Bearer) and
	// returns the facts, or an error for a 401.
	Authenticate func(*http.Request) (Facts, error)
	// Audit records an issue; an error refuses the token.
	Audit func(context.Context, Issued) error
	// Pushed is called after a manifest PUT succeeded (a build or a developer
	// finished a push), once per manifest, with the token's holder. Nil = none.
	Pushed func(context.Context, Pushed)
	// Gate, when closed, refuses every write with 503 and Retry-After: the
	// cleanup sweep is collecting garbage. Reads go through. Nil = never shut.
	Gate *Gate
	Now  func() time.Time

	once  sync.Once
	proxy *httputil.ReverseProxy
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/token":
		s.token(w, r)
	case r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/"):
		s.serveV2(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) challenge(w http.ResponseWriter, scopes []Scope, errCode string) {
	h := `Bearer realm="` + strings.TrimSuffix(s.Realm, "/") + `/token",service="` + Service + `"`
	if len(scopes) > 0 {
		var parts []string
		for _, sc := range scopes {
			parts = append(parts, "repository:"+sc.String())
		}
		h += `,scope="` + strings.Join(parts, " ") + `"`
	}
	if errCode != "" {
		h += `,error="` + errCode + `"`
	}
	w.Header().Set("WWW-Authenticate", h)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`))
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if v := q.Get("service"); v != "" && v != Service {
		http.Error(w, "unknown service", http.StatusBadRequest)
		return
	}
	f, err := s.Authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="stackr registry"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	granted := Decide(f, ParseScopes(q["scope"]))
	ttl := s.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	now := s.now()
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	c := Claims{
		ID: hex.EncodeToString(id), Kind: f.Kind, Subject: f.Subject,
		Issued: now.Unix(), Expires: now.Add(ttl).Unix(), Access: granted,
	}
	if s.Audit != nil {
		var sc []string
		for _, g := range granted {
			sc = append(sc, g.String())
		}
		rec := Issued{c.ID, f.Kind, f.Subject, strings.Join(sc, " "), now, now.Add(ttl)}
		if err := s.Audit(r.Context(), rec); err != nil {
			http.Error(w, "audit failed", http.StatusInternalServerError)
			return
		}
	}
	tok := Sign(s.Key, c)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token": tok, "access_token": tok,
		"expires_in": int(ttl.Seconds()), "issued_at": now.UTC().Format(time.RFC3339),
	})
}

// splitPath cuts a registry path (after /v2/) into repository, endpoint kind
// ("blobs", "manifests", "tags", "referrers") and the endpoint's last part,
// working from the end the way the registry's router does, so a repository
// of any depth (and one with a part named like an endpoint) splits the same
// here and there. ok is false for a path the registry API does not have.
func splitPath(rest string) (repo, kind, ref string, ok bool) {
	p := strings.Split(rest, "/")
	n := len(p)
	switch {
	case n >= 3 && p[n-1] == "list" && p[n-2] == "tags":
		return strings.Join(p[:n-2], "/"), "tags", "", true
	case n >= 4 && p[n-3] == "blobs" && p[n-2] == "uploads":
		return strings.Join(p[:n-3], "/"), "blobs", p[n-1], true
	case n >= 3 && slices.Contains([]string{"blobs", "manifests", "referrers"}, p[n-2]):
		return strings.Join(p[:n-2], "/"), p[n-2], p[n-1], true
	}
	return "", "", "", false
}

// uploadPath is a request for one upload session: .../blobs/uploads/<id>.
func uploadPath(path string) bool {
	p := strings.Split(strings.Trim(strings.TrimPrefix(path, "/v2"), "/"), "/")
	n := len(p)
	return n >= 4 && p[n-3] == "blobs" && p[n-2] == "uploads"
}

func validRepo(repo string) bool {
	if _, _, _, ok := ParseRepo(repo); ok || systemRepo(repo) {
		return true
	}
	_, _, ok := ParseImportRepo(repo)
	return ok
}

// required is what the request needs. ok is false for a path the registry
// API does not have.
func required(r *http.Request) (need []Scope, ok bool) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v2"), "/")
	if rest == "" {
		return nil, true // the version check: any valid token
	}
	if rest == "_catalog" {
		return []Scope{{CatalogRepo, []string{"*"}}}, true
	}
	repo, _, _, split := splitPath(rest)
	if !split || !validRepo(repo) {
		return nil, false
	}
	act := Pull
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		act = Push
	}
	return []Scope{{repo, []string{act}}}, true
}

// mountFrom is the source repo of a cross-repo blob mount, "" for none.
func mountFrom(r *http.Request) string {
	if r.Method != http.MethodPost {
		return ""
	}
	return r.URL.Query().Get("from")
}

// refuse is the registry API's error body with a status.
func refuse(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"errors":[{"code":"` + code + `","message":"` + msg + `"}]}`))
}

// formBody says the request carries a form or multipart body.
func formBody(r *http.Request) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return mt == "application/x-www-form-urlencoded" || strings.HasPrefix(mt, "multipart/")
}

func (s *Server) serveV2(w http.ResponseWriter, r *http.Request) {
	need, ok := required(r)
	if !ok {
		refuse(w, http.StatusNotFound, "NAME_UNKNOWN", "unknown repository")
		return
	}
	if _, kind, _, _ := splitPath(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v2"), "/")); kind != "" {
		switch {
		// The registry reads mount and from out of a form body as well as the
		// query (r.FormValue), past mountFrom's check.
		case r.Method == http.MethodPost && kind == "blobs" && formBody(r):
			refuse(w, http.StatusBadRequest, "UNSUPPORTED", "form bodies are not accepted on uploads")
			return
		// Push rights write. Deleting goes through the controller's cleanup,
		// straight to the registry; only cancelling an upload is left.
		case r.Method == http.MethodDelete && !uploadPath(r.URL.Path):
			refuse(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "delete is not served here")
			return
		}
	}
	bearer, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found {
		s.challenge(w, need, "")
		return
	}
	c, err := Verify(s.Key, bearer, s.now())
	if err != nil {
		s.challenge(w, need, "invalid_token")
		return
	}
	for _, n := range need {
		if !c.permits(n.Repo, n.Actions[0]) {
			s.challenge(w, need, "insufficient_scope")
			return
		}
	}
	if s.Gate != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		leave, open := s.Gate.TryEnter()
		if !open {
			w.Header().Set("Retry-After", gateRetryAfter)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"errors":[{"code":"UNAVAILABLE","message":"registry cleanup is running, retry shortly"}]}`))
			return
		}
		defer leave()
	}
	// A mount reads its source; without pull there, drop the mount and the
	// registry starts a plain upload instead, as the spec allows.
	if from := mountFrom(r); from != "" && !c.permits(from, Pull) {
		q := r.URL.Query()
		q.Del("mount")
		q.Del("from")
		r.URL.RawQuery = q.Encode()
	}
	s.once.Do(func() {
		s.proxy = &httputil.ReverseProxy{
			FlushInterval: -1, // layers stream
			Transport:     s.Transport,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(s.Backend)
				// The registry builds its upload URLs from the Host and what
				// Caddy forwarded, so both go through as they came.
				pr.Out.Host = pr.In.Host
				// Not Forwarded: Caddy sets none, so it is the client's own.
				for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
					if v, ok := pr.In.Header[h]; ok {
						pr.Out.Header[h] = v
					}
				}
				if ip, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
					if prior := pr.Out.Header.Values("X-Forwarded-For"); len(prior) > 0 {
						ip = strings.Join(prior, ", ") + ", " + ip
					}
					pr.Out.Header.Set("X-Forwarded-For", ip)
				}
				pr.Out.Header.Del("Authorization") // the registry needs none, and must not see ours
			},
		}
	})
	if s.Pushed == nil || r.Method != http.MethodPut {
		s.proxy.ServeHTTP(w, r)
		return
	}
	sw := &statusWriter{ResponseWriter: w}
	s.proxy.ServeHTTP(sw, r)
	repo, kind, ref, _ := splitPath(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v2"), "/"))
	if kind == "manifests" && sw.status/100 == 2 {
		s.Pushed(r.Context(), Pushed{
			Repo: repo, Reference: ref, Digest: w.Header().Get("Docker-Content-Digest"),
			Kind: c.Kind, Subject: c.Subject,
		})
	}
}

// statusWriter remembers the status the proxy answered with.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer (layers stream).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush keeps the proxy's streaming working through the wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
