package domain_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/standard"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The forward auth JSON is only worth its goldens if Caddy accepts it: build
// a full config, point it at free ports with the admin API off, and load it.
func TestForwardAuthLoads(t *testing.T) {
	off := install
	off.TLSOff = true
	for _, e := range []domain.Extras{
		{ForwardAuth: &domain.ForwardAuth{URL: "https://auth.example.com/api/verify"}},
		{
			ForwardAuth: &domain.ForwardAuth{URL: "http://authelia:9091", CopyHeaders: []string{"X-User"}},
			BasicAuth:   &domain.BasicAuth{User: "u", Password: "p"},
		},
	} {
		raw, err := domain.Build(off, one(row("app.example.com", e)), nil, expand)
		must(t, err)
		var cfg map[string]any
		must(t, json.Unmarshal(raw, &cfg))
		cfg["admin"] = map[string]any{"disabled": true}
		for _, s := range cfg["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any) {
			s.(map[string]any)["listen"] = []string{"127.0.0.1:0"}
		}
		out, err := json.Marshal(cfg)
		must(t, err)
		if err := caddy.Load(out, true); err != nil {
			t.Fatalf("caddy refused %s: %v", out, err)
		}
		must(t, caddy.Stop())
	}
}

func TestForwardAuthDefaultsAndOrder(t *testing.T) {
	raw, err := domain.Build(install, one(row("a.io", domain.Extras{
		ForwardAuth: &domain.ForwardAuth{URL: "https://auth.io"},
		BasicAuth:   &domain.BasicAuth{User: "u", Password: "p"},
	})), nil, expand)
	must(t, err)
	s := string(raw)
	for _, h := range []string{"Remote-User", "Remote-Groups", "Remote-Name", "Remote-Email"} {
		if !strings.Contains(s, "{http.reverse_proxy.header."+h+"}") {
			t.Errorf("default header %s not copied", h)
		}
	}
	if !strings.Contains(s, "auth.io:443") || strings.Index(s, "auth.io:443") < strings.Index(s, "http_basic") {
		t.Error("basic auth must come before forward auth")
	}
}

// proxyRun loads Build's output into a real in-process Caddy in front of a
// mock app and sends it one request. Tests using it must not run in parallel
// (Caddy's config is process-global).
type proxyRun struct {
	app   http.Header // what the app received; nil when it never ran
	body  string
	reply *http.Response
}

func proxy(t *testing.T, e domain.Extras, protect *domain.BasicAuth, req func(addr string) *http.Request) proxyRun {
	t.Helper()
	var out proxyRun
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		out.app, out.body = r.Header.Clone(), string(b)
		_, _ = w.Write([]byte("APP"))
	}))
	t.Cleanup(app.Close)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	addr := l.Addr().String()
	must(t, l.Close())

	d := row("app.example.com", e)
	d.ContainerPort = app.Listener.Addr().(*net.TCPAddr).Port
	off := install
	off.TLSOff = true
	tile := domain.TileRoute{TileID: "t1", Upstreams: []string{"127.0.0.1"}, Domains: []store.Domain{d}, Protect: protect}
	raw, err := domain.Build(off, []domain.TileRoute{tile}, nil, expand)
	must(t, err)
	var cfg map[string]any
	must(t, json.Unmarshal(raw, &cfg))
	cfg["admin"] = map[string]any{"disabled": true}
	for _, s := range cfg["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any) {
		s.(map[string]any)["listen"] = []string{addr}
	}
	b, err := json.Marshal(cfg)
	must(t, err)
	must(t, caddy.Load(b, true))
	t.Cleanup(func() { _ = caddy.Stop() })

	r := req(addr)
	r.Host = "app.example.com"
	out.reply, err = http.DefaultClient.Do(r)
	must(t, err)
	t.Cleanup(func() { _ = out.reply.Body.Close() })
	return out
}

func get(path string, hdr map[string]string) func(string) *http.Request {
	return func(addr string) *http.Request {
		r, _ := http.NewRequest("GET", "http://"+addr+path, nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
}

type provider struct {
	hits    int
	method  string
	uri     string
	body    string
	headers http.Header
}

func newProvider(t *testing.T, reply func(w http.ResponseWriter, r *http.Request)) (*provider, string) {
	p := &provider{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.hits++
		p.method, p.uri, p.body, p.headers = r.Method, r.URL.RequestURI(), string(b), r.Header.Clone()
		reply(w, r)
	}))
	t.Cleanup(s.Close)
	return p, s.URL
}

func ok200(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }

// A client-sent Remote-User must never survive a 2xx that omits it.
func TestForwardAuthSpoofedHeaderDeleted(t *testing.T) {
	_, url := newProvider(t, ok200)
	got := proxy(t, domain.Extras{ForwardAuth: &domain.ForwardAuth{URL: url + "/verify"}}, nil,
		get("/a", map[string]string{"Remote-User": "admin"}))
	if got.app == nil {
		t.Fatal("app never reached on a 2xx")
	}
	for k, v := range got.app {
		if strings.HasPrefix(k, "Remote-") || strings.Contains(strings.Join(v, ""), "{") {
			t.Errorf("app saw %s=%v", k, v)
		}
	}
}

// The provider's header reaches the app, also when the copy name is written
// in lower case.
func TestForwardAuthCopiesProviderHeader(t *testing.T) {
	_, url := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Remote-User", "alice")
		w.WriteHeader(200)
	})
	for _, names := range [][]string{nil, {"remote-user"}} {
		got := proxy(t, domain.Extras{ForwardAuth: &domain.ForwardAuth{URL: url + "/verify", CopyHeaders: names}}, nil,
			get("/a", map[string]string{"Remote-User": "admin"}))
		if u := got.app.Get("Remote-User"); u != "alice" {
			t.Errorf("copy %v: app saw Remote-User=%q", names, u)
		}
	}
}

// A URL without a path must not hand the client's path or body to the provider.
func TestForwardAuthNoPathAsksRoot(t *testing.T) {
	p, url := newProvider(t, ok200)
	proxy(t, domain.Extras{ForwardAuth: &domain.ForwardAuth{URL: url}}, nil, func(a string) *http.Request {
		req, _ := http.NewRequest("GET", "http://"+a+"/health?x=1", strings.NewReader("payload"))
		return req
	})
	if p.uri != "/" || p.method != "GET" || p.body != "" {
		t.Errorf("provider got %s %s body=%q", p.method, p.uri, p.body)
	}
}

// Client-chosen X-Original-* never reach the provider.
func TestForwardAuthDropsOriginalHeaders(t *testing.T) {
	p, url := newProvider(t, ok200)
	proxy(t, domain.Extras{ForwardAuth: &domain.ForwardAuth{URL: url + "/verify"}}, nil,
		get("/a", map[string]string{"X-Original-URL": "https://x/public", "X-Original-Method": "OPTIONS"}))
	for _, h := range []string{"X-Original-Url", "X-Original-Method"} {
		if v := p.headers.Get(h); v != "" {
			t.Errorf("provider saw %s=%q", h, v)
		}
	}
}

// Protect credentials are checked before the provider sees the request, so
// a visitor without them never reaches it and one with them is not leaked.
func TestForwardAuthAfterBasicAuth(t *testing.T) {
	p, url := newProvider(t, ok200)
	e := domain.Extras{ForwardAuth: &domain.ForwardAuth{URL: url + "/verify"}}
	basic := &domain.BasicAuth{User: "u", Password: "p"}
	got := proxy(t, e, basic, get("/a", nil))
	if p.hits != 0 || got.reply.StatusCode != 401 {
		t.Errorf("no credentials: provider hits=%d, status %d", p.hits, got.reply.StatusCode)
	}
}

func TestForwardAuthDoesNotLeakAuthorization(t *testing.T) {
	p, url := newProvider(t, ok200)
	e := domain.Extras{ForwardAuth: &domain.ForwardAuth{URL: url + "/verify"}}
	proxy(t, e, &domain.BasicAuth{User: "u", Password: "p"}, get("/a", map[string]string{"Authorization": "Basic dTpz"}))
	if p.hits != 0 {
		t.Errorf("wrong credentials reached the provider: %v", p.headers)
	}
}

// Strings that would reach Caddy as placeholders, bad header names and a URL
// that cannot be parsed all fail the build closed.
func TestBuildRefusesHostileStrings(t *testing.T) {
	for name, e := range map[string]domain.Extras{
		"url query":      {ForwardAuth: &domain.ForwardAuth{URL: "http://a.io/v?t={env.DNS_API_TOKEN}"}},
		"url path":       {ForwardAuth: &domain.ForwardAuth{URL: "http://a.io/{file./etc/x}"}},
		"copy name":      {ForwardAuth: &domain.ForwardAuth{URL: "http://a.io", CopyHeaders: []string{"{file./etc/x}"}}},
		"copy name junk": {ForwardAuth: &domain.ForwardAuth{URL: "http://a.io", CopyHeaders: []string{"X User"}}},
		"header key":     {Headers: map[string]string{"{env.X}": "v"}},
		"header value":   {Headers: map[string]string{"X-A": "{env.X}"}},
		"bad url":        {ForwardAuth: &domain.ForwardAuth{URL: "http://a.io/%zz"}},
	} {
		if _, err := domain.Build(install, one(row("a.io", e)), nil, expand); err == nil {
			t.Errorf("%s: Build accepted it", name)
		}
	}
}

func TestAttachRefusesBraces(t *testing.T) {
	l, _, web, _ := setup(t)
	for name, e := range map[string]domain.Extras{
		"url":          {ForwardAuth: &domain.ForwardAuth{URL: "http://a.io/v?t={env.X}"}},
		"copy name":    {ForwardAuth: &domain.ForwardAuth{URL: "http://a.io", CopyHeaders: []string{"{env.X}"}}},
		"header value": {Headers: map[string]string{"X-A": "{env.X}"}},
		"header key":   {Headers: map[string]string{"X-{A}": "v"}},
	} {
		if _, err := l.Attach(ctx, web, domain.Spec{Host: "h.example.com", Port: 80, Extras: e}, false, nil); err == nil {
			t.Errorf("%s: Attach accepted it", name)
		}
	}
}

func TestCheckBraces(t *testing.T) {
	if domain.CheckBraces("f", "plain") != nil || domain.CheckBraces("f", "a{b") == nil || domain.CheckBraces("f", "a}b") == nil {
		t.Error("CheckBraces must refuse { and } only")
	}
}

// An IPv6 provider is dialled as [::1]:port.
func TestForwardAuthDialsIPv6(t *testing.T) {
	raw, err := domain.Build(install, one(row("a.io", domain.Extras{
		ForwardAuth: &domain.ForwardAuth{URL: "http://[::1]:9091/v"},
	})), nil, expand)
	must(t, err)
	if !strings.Contains(string(raw), `"[::1]:9091"`) {
		t.Errorf("no bracketed dial in %s", raw)
	}
}
