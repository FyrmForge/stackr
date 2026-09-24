package registry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseRef(t *testing.T) {
	for _, c := range []struct {
		ref, host, repo, tag string
		ok                   bool
	}{
		{"nginx", "registry-1.docker.io", "library/nginx", "latest", true},
		{"docker.io/nginx", "registry-1.docker.io", "library/nginx", "latest", true},
		{"nginx:1.27", "registry-1.docker.io", "library/nginx", "1.27", true},
		{"ghcr.io/acme/api:v2", "ghcr.io", "acme/api", "v2", true},
		{"localhost:5000/app", "localhost:5000", "app", "latest", true},
		{"x@sha256:abc", "", "", "", false},
		{"", "", "", "", false},
	} {
		host, repo, tag, ok := ParseRef(c.ref)
		if host != c.host || repo != c.repo || tag != c.tag || ok != c.ok {
			t.Errorf("ParseRef(%q) = %q %q %q %v", c.ref, host, repo, tag, ok)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	s, p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`)
	if s != "bearer" || p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" {
		t.Errorf("bearer: %q %v", s, p)
	}
	if s, _ := parseChallenge(`Basic realm="x"`); s != "basic" {
		t.Errorf("basic: %q", s)
	}
}

// fakeRegistry answers like Docker Hub does (recorded shapes): anonymous
// manifest requests get a bearer challenge, the token endpoint hands out a
// token, and "priv" wants basic auth.
func fakeRegistry(t *testing.T) string {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if r.URL.Query().Get("scope") != "repository:app:pull" {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"token":"tok","expires_in":300}`))
			return
		case strings.HasPrefix(r.URL.Path, "/v2/priv/"):
			if u, p, ok := r.BasicAuth(); !ok || u != "u" || p != "p" {
				w.Header().Set("Www-Authenticate", `Basic realm="reg"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		case r.URL.Path == "/v2/limited/manifests/1":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		case r.Header.Get("Authorization") != "Bearer tok":
			w.Header().Set("Www-Authenticate", `Bearer realm="`+srv.URL+`/token",service="fake"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/app/manifests/1", "/v2/priv/manifests/1":
			if !strings.Contains(r.Header.Get("Accept"), "image.index") {
				http.Error(w, "index not accepted", http.StatusBadRequest)
				return
			}
			w.Header().Set("Docker-Content-Digest", "sha256:index")
		case "/v2/app/tags/list":
			if r.URL.Query().Get("last") == "" {
				w.Header().Set("Link", `</v2/app/tags/list?last=b&n=2>; rel="next"`)
				_, _ = w.Write([]byte(`{"name":"app","tags":["a","b"]}`))
			} else {
				_, _ = w.Write([]byte(`{"name":"app","tags":["c"]}`))
			}
		default:
			http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	old := HTTP
	HTTP = srv.Client()
	t.Cleanup(func() { HTTP = old })
	return strings.TrimPrefix(srv.URL, "https://")
}

func TestDigestAndTags(t *testing.T) {
	host := fakeRegistry(t)
	ctx := context.Background()

	if dg, err := Digest(ctx, host+"/app:1", "", ""); err != nil || dg != "sha256:index" {
		t.Fatalf("bearer digest: %q %v", dg, err)
	}
	if dg, err := Digest(ctx, host+"/priv:1", "u", "p"); err != nil || dg != "sha256:index" {
		t.Fatalf("basic digest: %q %v", dg, err)
	}
	if _, err := Digest(ctx, host+"/priv:1", "", ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("basic without creds: %v", err)
	}
	if _, err := Digest(ctx, host+"/app:gone", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing tag: %v", err)
	}
	var rl RateLimited
	if _, err := Digest(ctx, host+"/limited:1", "", ""); !errors.As(err, &rl) || rl.RetryAfter != 7*time.Second {
		t.Fatalf("rate limit: %v", err)
	}
	if _, err := Digest(ctx, "x@sha256:1", "", ""); !errors.Is(err, ErrBadRef) {
		t.Fatalf("pinned ref: %v", err)
	}
	tags, err := Tags(ctx, host+"/app", "", "")
	if err != nil || !slices.Equal(tags, []string{"a", "b", "c"}) {
		t.Fatalf("tags: %v %v", tags, err)
	}
}
