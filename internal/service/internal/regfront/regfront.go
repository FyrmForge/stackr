// Package regfront fronts the head's local registry. The registry itself
// (installspec.Registry) listens on loopback and checks nothing; this package
// speaks Docker's token auth to clients and enforces the token on every call
// it proxies.
//
// Image naming: one repository per tile, "<org>/<stack>/<tile>", each part a
// slug (lower-case letters, digits, '.', '_', '-'). Workers pull
// "localhost:5000/<org>/<stack>/<tile>:<tag>", developers
// "registry.<panel domain>/<org>/<stack>/<tile>:<tag>". The tag is the
// caller's (a build id); nothing here reads it. Exactly three path parts, so
// a request path splits unambiguously into repository and the registry's own
// "blobs", "manifests", "tags" and "referrers" endpoints.
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
	// PushRepos are the repos of the builder's build jobs; pushing implies
	// pulling them.
	PushRepos []string
}

func (f Facts) allowed(repo, action string) bool {
	if f.Kind == KindAdmin {
		return true
	}
	org, _, _, ok := ParseRepo(repo)
	if !ok {
		return false
	}
	if slices.Contains(f.Orgs, org) {
		return action == Pull || action == Push
	}
	if action == Pull && slices.Contains(f.PullOrgs, org) {
		return true
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

// Server is the front: the token endpoint at /token and the proxy under /v2/.
type Server struct {
	Key []byte
	// Realm is the front's public base URL, where /token is.
	Realm   string
	Backend *url.URL // the local registry
	TTL     time.Duration
	// Authenticate reads the request's credentials (Basic auth, a node
	// certificate) and returns the facts, or an error for a 401.
	Authenticate func(*http.Request) (Facts, error)
	// Audit records an issue; an error refuses the token.
	Audit func(context.Context, Issued) error
	Now   func() time.Time

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
	p := strings.SplitN(rest, "/", 4)
	if len(p) < 4 || !slices.Contains([]string{"blobs", "manifests", "tags", "referrers"}, p[3][:strings.IndexAny(p[3]+"/", "/")]) {
		return nil, false
	}
	repo := strings.Join(p[:3], "/")
	if _, _, _, valid := ParseRepo(repo); !valid {
		return nil, false
	}
	act := Pull
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		act = Push
	}
	need = []Scope{{repo, []string{act}}}
	// a cross-repo blob mount also reads the source
	if from := r.URL.Query().Get("from"); from != "" && r.Method == http.MethodPost {
		if _, _, _, valid := ParseRepo(from); !valid {
			return nil, false
		}
		need = append(need, Scope{from, []string{Pull}})
	}
	return need, true
}

func (s *Server) serveV2(w http.ResponseWriter, r *http.Request) {
	need, ok := required(r)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"code":"NAME_UNKNOWN","message":"unknown repository"}]}`))
		return
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
	s.once.Do(func() {
		s.proxy = httputil.NewSingleHostReverseProxy(s.Backend)
		s.proxy.FlushInterval = -1 // layers stream
		d := s.proxy.Director
		s.proxy.Director = func(r *http.Request) {
			d(r)
			r.Header.Del("Authorization") // the registry needs none, and must not see ours
		}
	})
	s.proxy.ServeHTTP(w, r)
}
