package domain_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/standard"
	_ "github.com/mholt/caddy-l4"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
)

var (
	pass     = domain.Route{Host: "pve.example.com", Mode: "passthrough", Target: "10.0.0.5:443"}
	plainRt  = domain.Route{Host: "hole.example.com", Mode: "http", Target: "10.0.0.6:80"}
	secureRt = domain.Route{Host: "nas.example.com", Mode: "https", Target: "10.0.0.7:443"}
	skipRt   = domain.Route{Host: "px.example.com", Mode: "https", Target: "10.0.0.8:8006", Insecure: true}
	wildPass = domain.Route{Host: "*.fyrm.io", Mode: "passthrough", Target: "10.0.0.9:443"}
)

func TestRoutesGolden(t *testing.T) {
	for _, g := range []struct {
		name   string
		in     domain.Install
		routes []domain.Route
	}{
		{"routes_passthrough", install, []domain.Route{pass}},
		{"routes_http", install, []domain.Route{plainRt}},
		{"routes_https_insecure", install, []domain.Route{secureRt, skipRt}},
		{"routes_wrapper_order", install, []domain.Route{wildPass, pass, plainRt}},
		{"routes_tls_off", domain.Install{AdminListen: "0.0.0.0:2019", TLSOff: true}, []domain.Route{pass, secureRt}},
	} {
		t.Run(g.name, func(t *testing.T) {
			cfg, err := domain.Build(g.in, one(row("app.example.com", domain.Extras{})), g.routes, nil)
			must(t, err)
			var buf bytes.Buffer
			must(t, json.Indent(&buf, cfg, "", "  "))
			file := filepath.Join("testdata", g.name+".json")
			if *update {
				must(t, os.WriteFile(file, append(buf.Bytes(), '\n'), 0o644))
			}
			want, err := os.ReadFile(file)
			must(t, err)
			if string(want) != buf.String()+"\n" {
				t.Errorf("%s differs:\n%s", file, buf.Bytes())
			}
		})
	}
}

// The layer4 wrapper sits before the tls wrapper, exact hosts before
// wildcards; a pass-through host is never a certificate subject.
func TestPassthroughWrapper(t *testing.T) {
	in := install
	in.Accounts = []domain.Account{{Host: "example.com", Email: "dns@example.com"}}
	cfg, err := domain.Build(in, nil, []domain.Route{wildPass, pass, secureRt}, nil)
	must(t, err)
	var c struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Wrappers []map[string]any `json:"listener_wrappers"`
				}
			}
			TLS struct {
				Automation struct{ Policies []struct{ Subjects []string } }
			}
		}
	}
	must(t, json.Unmarshal(cfg, &c))
	ws := c.Apps.HTTP.Servers["https"].Wrappers
	if len(ws) != 2 || ws[0]["wrapper"] != "layer4" || ws[1]["wrapper"] != "tls" {
		t.Fatalf("wrappers = %v, want layer4 then tls", ws)
	}
	if got := strings.Count(string(cfg), `"sni"`); got != 2 {
		t.Errorf("%d sni matchers, want 2", got)
	}
	if strings.Index(string(cfg), `"pve.example.com"]}}`) > strings.Index(string(cfg), `"*.fyrm.io"]}}`) {
		t.Errorf("exact host after the wildcard in the wrapper")
	}
	var subjects []string
	for _, p := range c.Apps.TLS.Automation.Policies {
		subjects = append(subjects, p.Subjects...)
	}
	if strings.Join(subjects, ",") != "nas.example.com" {
		t.Errorf("certificate subjects = %v, want only the https route", subjects)
	}
}

// No pass-through, no wrapper: the https server keeps Caddy's default
// listener wrappers.
func TestNoWrapperWithoutPassthrough(t *testing.T) {
	cfg, err := domain.Build(install, nil, []domain.Route{secureRt}, nil)
	must(t, err)
	if strings.Contains(string(cfg), "listener_wrappers") {
		t.Errorf("wrapper set without a pass-through route: %s", cfg)
	}
}

// A pass-through route beside an https one is only worth its goldens if real
// Caddy, with the layer4 module imported, accepts the config.
func TestRoutesLoad(t *testing.T) {
	raw, err := domain.Build(install, one(row("app.example.com", domain.Extras{})), []domain.Route{pass, secureRt}, nil)
	must(t, err)
	var cfg map[string]any
	must(t, json.Unmarshal(raw, &cfg))
	cfg["admin"] = map[string]any{"disabled": true}
	for name, s := range cfg["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any) {
		srv := s.(map[string]any)
		srv["listen"] = []string{"127.0.0.1:0"}
		if name == "https" {
			srv["listen"] = []string{"127.0.0.2:0"} // two servers may not share an address
		}
		srv["automatic_https"] = map[string]any{"disable": true} // no ACME from a test
	}
	out, err := json.Marshal(cfg)
	must(t, err)
	if err := caddy.Load(out, true); err != nil {
		t.Fatalf("caddy refused %s: %v", out, err)
	}
	must(t, caddy.Stop())
}
