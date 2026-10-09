package domain_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
)

const customRoute = `{"match":[{"host":["extra.example.com"]}],"handle":[{"handler":"static_response","body":"hi"}]}`

// serverRoutes is one Caddy server's route list, as raw JSON.
func serverRoutes(t *testing.T, cfg []byte, srv string) []json.RawMessage {
	t.Helper()
	var c struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Routes []json.RawMessage `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	return c.Apps.HTTP.Servers[srv].Routes
}

// proxy_custom is a JSON array of Caddy routes appended after stackr's own on
// the main server (https, or http when TLS is off): additive, so the panel
// vhost and the tiles stay first; only the unknown-host 404 comes after.
func TestCustomRoutesAppended(t *testing.T) {
	in := install
	in.PanelHost, in.PanelUpstream = "panel.example.com", "stackrd:8080"
	in.Custom = "[" + customRoute + "]"
	cfg, err := domain.Build(in, one(row("app.example.com", domain.Extras{})), nil, expand)
	if err != nil {
		t.Fatal(err)
	}
	secure := serverRoutes(t, cfg, "https")
	if n := len(secure); n != 4 {
		t.Fatalf("https routes = %d, want panel, tile, custom, 404:\n%s", n, cfg)
	}
	if !strings.Contains(string(secure[0]), "stackrd:8080") || !bytes.Equal(secure[2], []byte(customRoute)) ||
		!strings.Contains(string(secure[3]), `"status_code":404`) {
		t.Errorf("panel must stay first and the custom route last before the 404:\n%s", cfg)
	}
	for _, r := range serverRoutes(t, cfg, "http") {
		if strings.Contains(string(r), `"body":"hi"`) { // its host may redirect; the route may not
			t.Errorf("the custom route leaked onto the http server:\n%s", cfg)
		}
	}

	in.TLSOff = true
	cfg, err = domain.Build(in, nil, nil, expand)
	if err != nil {
		t.Fatal(err)
	}
	plain := serverRoutes(t, cfg, "http")
	if len(plain) != 3 || !bytes.Equal(plain[1], []byte(customRoute)) {
		t.Errorf("TLS off: custom route not last before the 404 on http:\n%s", cfg)
	}
}

// A custom route's hosts follow the same certificate rules as the rest: the
// domain resource's ACME account covers them.
func TestCustomHostsGetCertPolicy(t *testing.T) {
	in := install
	in.Accounts = []domain.Account{{Host: "example.com", Email: "dns@example.com"}}
	in.Custom = "[" + customRoute + "]"
	cfg, err := domain.Build(in, nil, nil, expand)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), `"subjects":["extra.example.com"]`) || !strings.Contains(string(cfg), "dns@example.com") {
		t.Errorf("custom host not on its resource's account:\n%s", cfg)
	}
}

// Bad proxy_custom never blocks the rest: the config is still whole and the
// error names the setting, as a broken tile does.
func TestCustomRejected(t *testing.T) {
	for _, bad := range []string{`{"a":1}`, `[1]`, `[`, `[{"a":1}] x`, `"x"`} {
		in := install
		in.Custom = bad
		cfg, err := domain.Build(in, one(row("app.example.com", domain.Extras{})), nil, expand)
		if err == nil || !strings.Contains(err.Error(), "proxy_custom") {
			t.Errorf("%q: err = %v, want one naming proxy_custom", bad, err)
		}
		if !bytes.Contains(cfg, []byte("app.example.com")) {
			t.Errorf("%q: tiles lost with the bad custom config:\n%s", bad, cfg)
		}
	}
	for _, empty := range []string{"", "  \n", "[]"} {
		in := install
		in.Custom = empty
		if _, err := domain.Build(in, nil, nil, expand); err != nil {
			t.Errorf("%q refused: %v", empty, err)
		}
	}
}

// A redirect a rename generated (Auto) answers 308, one the stack file
// declared 301.
func TestGeneratedRedirectIs308(t *testing.T) {
	for _, c := range []struct {
		auto bool
		code string
	}{{true, `"status_code":308`}, {false, `"status_code":301`}} {
		d := row("old.example.com", domain.Extras{})
		d.RedirectTo, d.Auto = "new.example.com", c.auto
		cfg, err := domain.Build(install, one(d), nil, expand)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(cfg, []byte(c.code)) || !bytes.Contains(cfg, []byte("https://new.example.com")) {
			t.Errorf("auto=%v: want %s in\n%s", c.auto, c.code, cfg)
		}
	}
}

// With TLS on, a custom route's host gets its certificate and a :80 route
// that bounces it onto https, like every other host.
func TestCustomHostRedirectsOnPlain(t *testing.T) {
	in := install
	in.Custom = "[" + customRoute + "]"
	cfg, err := domain.Build(in, nil, nil, expand)
	if err != nil {
		t.Fatal(err)
	}
	var redirect bool
	for _, r := range serverRoutes(t, cfg, "http") {
		s := string(r)
		redirect = redirect || strings.Contains(s, "extra.example.com") && strings.Contains(s, `"status_code":308`)
		if strings.Contains(s, `"body":"hi"`) {
			t.Errorf("the custom route itself leaked onto http:\n%s", cfg)
		}
	}
	if !redirect {
		t.Errorf("no :80 redirect for the custom host:\n%s", cfg)
	}
}
