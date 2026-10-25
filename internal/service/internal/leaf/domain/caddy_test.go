package domain_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

var update = flag.Bool("update", false, "rewrite testdata/*.json")

var install = domain.Install{AdminListen: "0.0.0.0:2019", ACMEEmail: "ops@stackr.io"}

func row(host string, e domain.Extras) store.Domain {
	b, _ := json.Marshal(e)
	return store.Domain{
		Host:          host,
		ContainerPort: 8080,
		HTTPS:         true,
		ForceHTTPS:    true,
		ProxyJSON:     string(b),
	}
}

func one(d store.Domain) []domain.TileRoute {
	return []domain.TileRoute{{TileID: "t1", Upstreams: []string{"web-r2", "web-r1"}, Domains: []store.Domain{d}}}
}

func expand(s string) (string, error) {
	if s == "${{ secrets.web.pw }}" {
		return "s3cret", nil
	}
	return "", errors.New("unset")
}

type golden struct {
	name  string
	in    domain.Install
	tiles []domain.TileRoute
}

func goldens() []golden {
	path := row("app.example.com", domain.Extras{StripPrefix: true})
	path.Path = "/api"
	noForce := row("app.example.com", domain.Extras{})
	noForce.ForceHTTPS = false
	redirect := row(
		"old.example.com",
		domain.Extras{
			SecHeaders: true,
			BasicAuth:  &domain.BasicAuth{User: "u", Password: "p"},
		},
	)
	redirect.RedirectTo = "app.example.com"
	raw := row("app.example.com", domain.Extras{})
	raw.RawCaddy = `{"match":[{"host":["app.example.com"]}],"handle":[{"handler":"file_server","browse":{}}]}`
	protect := one(row("app.example.com", domain.Extras{}))
	protect[0].Protect = &domain.BasicAuth{User: "team", Password: "${{ secrets.web.pw }}"}
	health := one(row("app.example.com", domain.Extras{}))
	health[0].HealthPath = "/healthz"
	down := one(row("app.example.com", domain.Extras{}))
	down[0].Upstreams = nil
	off := install
	off.TLSOff = true
	panel := install
	panel.PanelHost, panel.PanelUpstream, panel.TrustedProxies = "panel.example.com", "stackrd:8080", []string{
		"10.0.0.0/8",
		"10.0.0.0/8",
		"2001:db8::/32",
	}
	wild := install
	wild.Managed = []string{"example.com"}
	wild.Accounts = []domain.Account{
		{Host: "example.com", Email: "a@example.com"},
		{Host: "shop.example.com", Email: "shop@example.com"},
	}
	many := []domain.TileRoute{{
		TileID:    "t1",
		Upstreams: []string{"web-r1"},
		Domains: []store.Domain{
			row("example.com", domain.Extras{}),
			row("a.shop.example.com", domain.Extras{}),
			row("*.example.com", domain.Extras{}),
			row("other.io", domain.Extras{}),
		},
	}}

	managed := install
	managed.Managed = []string{"other.io", "example.com"}

	logged := install
	logged.AccessLog, logged.AccessIP = true, true
	everything := logged
	everything.AccessQuery, everything.AccessHeaders = true, true

	fa := &domain.ForwardAuth{URL: "https://auth.example.com/api/verify"}
	return []golden{
		{"forward_auth", install, one(row("app.example.com", domain.Extras{ForwardAuth: fa}))},
		{"forward_auth_basic", install, one(row("app.example.com", domain.Extras{
			ForwardAuth: &domain.ForwardAuth{URL: "http://authelia:9091", CopyHeaders: []string{"X-User"}},
			BasicAuth:   &domain.BasicAuth{User: "u", Password: "${{ secrets.web.pw }}"},
		}))},
		{"access_log", logged, one(row("app.example.com", domain.Extras{}))},
		{"access_log_all", everything, one(row("app.example.com", domain.Extras{}))},
		{"plain", install, one(row("app.example.com", domain.Extras{}))},
		{"basic_auth", install, one(row("app.example.com", domain.Extras{
			BasicAuth: &domain.BasicAuth{User: "u", Password: "${{ secrets.web.pw }}"},
		}))},
		{"basic_auth_locked", install, one(row("app.example.com", domain.Extras{
			BasicAuth: &domain.BasicAuth{User: "u", Password: "${{ secrets.gone }}"},
		}))},
		{"protect_cascade", install, protect},
		{"websockets", install, one(row("app.example.com", domain.Extras{Websockets: true}))},
		{"max_body", install, one(row("app.example.com", domain.Extras{MaxBodyMB: 100}))},
		{"timeouts", install, one(row("app.example.com", domain.Extras{
			Timeouts: &domain.Timeouts{Dial: 5, Read: 300, Write: 300},
		}))},
		{"headers", install, one(row("app.example.com", domain.Extras{
			Headers:    map[string]string{"X-Robots-Tag": "noindex"},
			SecHeaders: true,
		}))},
		{"methods", install, one(row("dav.example.com", domain.Extras{Methods: []string{"GET", "PROPFIND", "MKCOL"}}))},
		{"strip_prefix", install, one(path)},
		{"sec_headers", install, one(row("app.example.com", domain.Extras{SecHeaders: true}))},
		{"raw", install, one(raw)},
		{"redirect", install, one(redirect)},
		{"no_force_https", install, one(noForce)},
		{"health", install, health},
		{"no_replicas", install, down},
		{"tls_off", off, one(row("app.example.com", domain.Extras{}))},
		{"panel_trusted", panel, one(row("app.example.com", domain.Extras{}))},
		{"accounts_wildcard", wild, many},
		{"managed_domains", managed, one(row("app.example.com", domain.Extras{}))},
	}
}

// A wildcard under a managed domain asks stackrd for its DNS-01 records; no
// config ever carries a provider key or an env placeholder for one.
func TestDNSChallengeHasNoToken(t *testing.T) {
	for _, g := range goldens() {
		cfg, err := domain.Build(g.in, g.tiles, nil, expand)
		must(t, err)
		s := string(cfg)
		if strings.Contains(s, "{env.") || strings.Contains(s, "token") || strings.Contains(s, "cloudflare") {
			t.Errorf("%s: a provider secret in the config: %s", g.name, s)
		}
		if n := strings.Count(s, `"dns":`); n != strings.Count(s, `"dns":{"provider":{"name":"stackr"}}`) {
			t.Errorf("%s: a DNS challenge not through stackr: %s", g.name, s)
		}
		if len(g.in.Managed) > 0 && !strings.Contains(s, `"provider":{"name":"stackr"}`) {
			t.Errorf("%s: managed domains but no stackr DNS challenge", g.name)
		}
	}
}

// A wildcard under no managed domain stays on the catch-all: nothing could
// answer its DNS-01 challenge.
func TestUnmanagedWildcardNoDNS(t *testing.T) {
	in := install
	in.Managed = []string{"example.com"}
	cfg, err := domain.Build(in, one(row("*.other.io", domain.Extras{})), nil, nil)
	must(t, err)
	var c struct {
		Apps struct {
			TLS struct {
				Automation struct {
					Policies []struct {
						Subjects []string `json:"subjects"`
					} `json:"policies"`
				} `json:"automation"`
				Certificates struct {
					Automate []string `json:"automate"`
				} `json:"certificates"`
			} `json:"tls"`
		} `json:"apps"`
	}
	must(t, json.Unmarshal(cfg, &c))
	ps := c.Apps.TLS.Automation.Policies
	if len(ps) != 1 || len(ps[0].Subjects) != 0 {
		t.Errorf("policies = %+v", ps)
	}
	if got := strings.Join(c.Apps.TLS.Certificates.Automate, ","); got != "" {
		t.Errorf("automate = %q", got)
	}
}

var bcryptRe = regexp.MustCompile(`\$2a\$05\$[./A-Za-z0-9]{53}`)

// One golden file per extra; bcrypt hashes are masked and checked apart.
func TestGolden(t *testing.T) {
	for _, g := range goldens() {
		t.Run(g.name, func(t *testing.T) {
			cfg, err := domain.Build(g.in, g.tiles, nil, expand)
			if err != nil {
				t.Fatal(err)
			}
			var parsed struct {
				Admin struct{ Listen string } `json:"admin"`
			}
			if json.Unmarshal(cfg, &parsed) != nil || parsed.Admin.Listen != g.in.AdminListen {
				t.Errorf("admin listener missing: %s", cfg)
			}
			var buf bytes.Buffer
			must(t, json.Indent(&buf, cfg, "", "  "))
			got := bcryptRe.ReplaceAll(buf.Bytes(), []byte("<bcrypt>"))
			file := filepath.Join("testdata", g.name+".json")
			if *update {
				must(t, os.WriteFile(file, append(got, '\n'), 0o644))
			}
			want, err := os.ReadFile(file)
			must(t, err)
			if string(want) != string(got)+"\n" {
				t.Errorf("%s differs:\n%s", file, got)
			}
		})
	}
}

func hashIn(t *testing.T, cfg []byte) (user, hash string) {
	t.Helper()
	m := regexp.MustCompile(`"password":"([^"]+)","username":"([^"]+)"`).FindSubmatch(cfg)
	if m == nil {
		t.Fatalf("no account in %s", cfg)
	}
	return string(m[2]), string(m[1])
}

// Basic auth fails closed: a ref that will not resolve, an empty password or
// one over bcrypt's 72 bytes all lock the route with a real hash.
func TestBasicAuthFailsClosed(t *testing.T) {
	build := func(pw string) []byte {
		cfg, err := domain.Build(
			install,
			one(row("a.io", domain.Extras{BasicAuth: &domain.BasicAuth{User: "u", Password: pw}})),
			nil,
			expand,
		)
		must(t, err)
		return cfg
	}
	user, hash := hashIn(t, build("${{ secrets.web.pw }}"))
	if user != "u" || bcrypt.CompareHashAndPassword([]byte(hash), []byte("s3cret")) != nil {
		t.Errorf("real hash: %s %s", user, hash)
	}
	for _, pw := range []string{"${{ secrets.gone }}", "", strings.Repeat("x", 73)} {
		user, hash := hashIn(t, build(pw))
		if user != "locked" || !strings.HasPrefix(hash, "$2a$05$") {
			t.Errorf("%q: %s %s, want the lock", pw, user, hash)
		}
	}
	if !bytes.Equal(build("plain"), build("plain")) {
		t.Error("a rebuild changed the bytes")
	}
}

// auto only marks a generated host; the route is the same.
func TestAutoRendersSame(t *testing.T) {
	a, b := row("a.io", domain.Extras{SecHeaders: true}), row("a.io", domain.Extras{SecHeaders: true})
	b.Auto = true
	ca, _ := domain.Build(install, one(a), nil, expand)
	cb, _ := domain.Build(install, one(b), nil, expand)
	if !bytes.Equal(ca, cb) {
		t.Errorf("auto changed the render:\n%s\n%s", ca, cb)
	}
}

func TestBrokenTileLeftOut(t *testing.T) {
	bad := row("bad.io", domain.Extras{})
	bad.ProxyJSON = "{"
	tiles := append(
		one(row("good.io", domain.Extras{})),
		domain.TileRoute{TileID: "t-bad", Domains: []store.Domain{bad}},
	)
	cfg, err := domain.Build(install, tiles, nil, expand)
	if err == nil || !strings.Contains(err.Error(), "t-bad") {
		t.Errorf("err = %v", err)
	}
	if !bytes.Contains(cfg, []byte("good.io")) || bytes.Contains(cfg, []byte("bad.io")) {
		t.Errorf("cfg = %s", cfg)
	}
	if _, err := domain.Build(domain.Install{}, nil, nil, nil); err == nil {
		t.Error("a config without the admin listener built")
	}
}

// Access logs are off unless asked for; by default they drop the query
// string, every request header (User-Agent returns as its own field) and
// keep the client address; each flag flips its own part.
func TestAccessLogFilters(t *testing.T) {
	build := func(mod func(*domain.Install)) string {
		in := install
		in.AccessLog, in.AccessIP = true, true
		mod(&in)
		cfg, err := domain.Build(in, one(row("app.example.com", domain.Extras{})), nil, expand)
		must(t, err)
		return strings.ReplaceAll(string(cfg), `\u003e`, ">") // json escapes >
	}
	has := func(t *testing.T, cfg, s string, want bool) {
		t.Helper()
		if strings.Contains(cfg, s) != want {
			t.Errorf("contains %s = %v, want %v", s, !want, want)
		}
	}
	const (
		query   = `"request>uri":{"filter":"regexp"`
		headers = `"request>headers":{"filter":"delete"}`
		ip      = `"request>client_ip":{"filter":"delete"}`
		ua      = `"key":"user_agent"`
	)
	t.Run("off", func(t *testing.T) {
		cfg, err := domain.Build(install, one(row("app.example.com", domain.Extras{})), nil, expand)
		must(t, err)
		has(t, string(cfg), `"logging"`, false)
		has(t, string(cfg), `"logs"`, false)
	})
	t.Run("defaults", func(t *testing.T) {
		cfg := build(func(*domain.Install) {})
		has(t, cfg, `"include":["http.log.access"]`, true)
		has(t, cfg, query, true)
		has(t, cfg, headers, true)
		has(t, cfg, ua, true)
		has(t, cfg, ip, false)
		has(t, cfg, `"logs":{}`, true)
	})
	t.Run("query", func(t *testing.T) {
		has(t, build(func(in *domain.Install) { in.AccessQuery = true }), query, false)
	})
	t.Run("headers", func(t *testing.T) {
		cfg := build(func(in *domain.Install) { in.AccessHeaders = true })
		has(t, cfg, headers, false)
		has(t, cfg, ua, false)
	})
	t.Run("no ip", func(t *testing.T) {
		has(t, build(func(in *domain.Install) { in.AccessIP = false }), ip, true)
	})
}

// IPv4Only binds the servers to 0.0.0.0; off, they listen on every address.
func TestBuildListensOnIPv4Only(t *testing.T) {
	for _, only := range []bool{false, true} {
		in := install
		in.IPv4Only = only
		cfg, err := domain.Build(in, one(row("app.example.com", domain.Extras{})), nil, expand)
		if err != nil {
			t.Fatal(err)
		}
		c := string(cfg)
		want, other := `":80"`, `"0.0.0.0:80"`
		if only {
			want, other = other, want
		}
		if !strings.Contains(c, want) || strings.Contains(c, other) {
			t.Errorf("IPv4Only=%v: listen not %s: %s", only, want, c)
		}
		if got := strings.Contains(c, `"0.0.0.0:443"`); got != only {
			t.Errorf("IPv4Only=%v: 0.0.0.0:443 present = %v", only, got)
		}
	}
}
