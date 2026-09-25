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
	return store.Domain{Host: host, ContainerPort: 8080, HTTPS: true, ForceHTTPS: true, ProxyJSON: string(b)}
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
	redirect := row("old.example.com", domain.Extras{SecHeaders: true, BasicAuth: &domain.BasicAuth{User: "u", Password: "p"}})
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
	panel.PanelHost, panel.PanelUpstream, panel.TrustedProxies = "panel.example.com", "stackrd:8080", []string{"10.0.0.0/8", "10.0.0.0/8", "2001:db8::/32"}
	wild := install
	wild.DNSProvider = "cloudflare"
	wild.Accounts = []domain.Account{{Host: "example.com", Email: "a@example.com"}, {Host: "shop.example.com", Email: "shop@example.com"}}
	many := []domain.TileRoute{{TileID: "t1", Upstreams: []string{"web-r1"}, Domains: []store.Domain{
		row("example.com", domain.Extras{}), row("a.shop.example.com", domain.Extras{}),
		row("*.example.com", domain.Extras{}), row("other.io", domain.Extras{}),
	}}}

	return []golden{
		{"plain", install, one(row("app.example.com", domain.Extras{}))},
		{"basic_auth", install, one(row("app.example.com", domain.Extras{BasicAuth: &domain.BasicAuth{User: "u", Password: "${{ secrets.web.pw }}"}}))},
		{"basic_auth_locked", install, one(row("app.example.com", domain.Extras{BasicAuth: &domain.BasicAuth{User: "u", Password: "${{ secrets.gone }}"}}))},
		{"protect_cascade", install, protect},
		{"websockets", install, one(row("app.example.com", domain.Extras{Websockets: true}))},
		{"max_body", install, one(row("app.example.com", domain.Extras{MaxBodyMB: 100}))},
		{"timeouts", install, one(row("app.example.com", domain.Extras{Timeouts: &domain.Timeouts{Dial: 5, Read: 300, Write: 300}}))},
		{"headers", install, one(row("app.example.com", domain.Extras{Headers: map[string]string{"X-Robots-Tag": "noindex"}, SecHeaders: true}))},
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
	}
}

var bcryptRe = regexp.MustCompile(`\$2a\$05\$[./A-Za-z0-9]{53}`)

// One golden file per extra; bcrypt hashes are masked and checked apart.
func TestGolden(t *testing.T) {
	for _, g := range goldens() {
		t.Run(g.name, func(t *testing.T) {
			cfg, err := domain.Build(g.in, g.tiles, expand)
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
		cfg, err := domain.Build(install, one(row("a.io", domain.Extras{BasicAuth: &domain.BasicAuth{User: "u", Password: pw}})), expand)
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
	ca, _ := domain.Build(install, one(a), expand)
	cb, _ := domain.Build(install, one(b), expand)
	if !bytes.Equal(ca, cb) {
		t.Errorf("auto changed the render:\n%s\n%s", ca, cb)
	}
}

func TestBrokenTileLeftOut(t *testing.T) {
	bad := row("bad.io", domain.Extras{})
	bad.ProxyJSON = "{"
	tiles := append(one(row("good.io", domain.Extras{})), domain.TileRoute{TileID: "t-bad", Domains: []store.Domain{bad}})
	cfg, err := domain.Build(install, tiles, expand)
	if err == nil || !strings.Contains(err.Error(), "t-bad") {
		t.Errorf("err = %v", err)
	}
	if !bytes.Contains(cfg, []byte("good.io")) || bytes.Contains(cfg, []byte("bad.io")) {
		t.Errorf("cfg = %s", cfg)
	}
	if _, err := domain.Build(domain.Install{}, nil, nil); err == nil {
		t.Error("a config without the admin listener built")
	}
}
