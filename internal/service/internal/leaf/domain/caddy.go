package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// DNSTokenEnv is the proxy container's env var holding the DNS-01 API token.
const DNSTokenEnv = "DNS_API_TOKEN"

// Install is the install-wide half of the config: facts from settings and
// the domain resources' ACME accounts.
type Install struct {
	AdminListen    string // required on every config, or Caddy falls back to its own localhost
	ACMEEmail      string // the instance account
	Accounts       []Account
	DNSProvider    string // "" = no DNS-01, so no wildcard certificates
	TLSOff         bool   // STACKR_TLS=off: plain HTTP only, no certificates
	TrustedProxies []string
	PanelHost      string // the panel's public host; skipped when a bare IP, localhost or ""
	PanelUpstream  string // the panel's internal dial address, e.g. stackrd:8080
	// Custom is proxy_custom: a JSON array of Caddy HTTP route objects,
	// appended after every route of ours on the main server (https, or http
	// with TLS off). Additive, never a whole-config replace, so the panel
	// vhost and the tiles always survive it.
	Custom string
}

// Account is a domain resource that names its own ACME email. A host
// uses the account of the longest base it equals or ends in.
type Account struct{ Host, Email string }

// TileRoute is one exposed tile: its rows plus the facts the rows lack.
type TileRoute struct {
	TileID     string
	Upstreams  []string   // replica names on the tile's ingress network
	HealthPath string     // "" = no active health check
	Protect    *BasicAuth // the settings cascade's answer, when a domain carries none
	Domains    []store.Domain
	Expand     Expand // this tile's refs; nil = Build's
}

// Expand resolves ${{ }} refs in a basic-auth password (params' resolver).
type Expand func(string) (string, error)

type route = map[string]any

type placed struct {
	host, path string
	r          any
}

// Build turns every exposed tile and every external route into one whole
// Caddy config. A tile whose rows will not parse is left out (a dead route
// beats a wrong one) and named in the error; the config is still good to
// push.
func Build(in Install, tiles []TileRoute, routes []Route, expand Expand) (json.RawMessage, error) {
	if in.AdminListen == "" {
		return nil, errors.New("domain: the Caddy config needs the admin listen address")
	}
	tlsOn := !in.TLSOff
	var plain, secure []placed
	var bad []error
	httpsHosts := map[string]bool{}

	if h := in.PanelHost; h != "" && h != "localhost" && net.ParseIP(h) == nil && in.PanelUpstream != "" {
		serve := route{
			"match":    []route{{"host": []string{h}}},
			"terminal": true,
			"handle":   []route{{"handler": "reverse_proxy", "upstreams": []route{{"dial": in.PanelUpstream}}}},
		}
		if tlsOn {
			secure = append(secure, placed{"", "", serve})
			plain = append(plain, placed{"", "", forceRoute(h, "")})
			httpsHosts[h] = true
		} else {
			plain = append(plain, placed{"", "", serve})
		}
	}

	for _, t := range tiles {
		var p, s []placed
		var err error
		for _, d := range t.Domains {
			var serve any
			if serve, err = domainRoute(d, t, expand, tlsOn); err != nil {
				break
			}
			secured := tlsOn && d.HTTPS
			if secured {
				s = append(s, placed{d.Host, d.Path, serve})
				httpsHosts[d.Host] = true
			}
			if secured && d.ForceHTTPS && d.RedirectTo == "" {
				p = append(p, placed{d.Host, d.Path, forceRoute(d.Host, d.Path)})
			} else {
				p = append(p, placed{d.Host, d.Path, serve})
			}
		}
		if err != nil {
			bad = append(bad, fmt.Errorf("tile %s: %w", t.TileID, err))
			continue
		}
		plain, secure = append(plain, p...), append(secure, s...)
	}

	// Routes to other machines (admin-made) sit after the tiles; the sort in
	// server orders them with the rest.
	rp, rs, wrapper := externalRoutes(routes, tlsOn)
	plain, secure = append(plain, rp...), append(secure, rs...)
	for _, p := range rs {
		if p.host != "" {
			httpsHosts[p.host] = true
		}
	}

	custom, err := customRoutes(in.Custom)
	if err != nil {
		bad = append(bad, err) // left out like a broken tile; the rest still pushes
	}
	httpTail := custom
	if tlsOn {
		httpTail = nil
		for _, h := range customHosts(custom) {
			if !httpsHosts[h] { // a host stackr already routes has its :80 route
				plain = append(plain, placed{h, "", forceRoute(h, "")})
			}
			httpsHosts[h] = true // the same certificate rules as every other host
		}
	}

	servers := route{"http": server(":80", plain, in.TrustedProxies, httpTail)}
	apps := route{"http": route{"servers": servers}}
	if tlsOn {
		servers["https"] = server(":443", secure, in.TrustedProxies, custom)
		servers["https"].(route)["automatic_https"] = route{"disable_redirects": true}
		if wrapper != nil {
			// The layer4 wrapper reads the SNI before TLS terminates; the
			// tls wrapper must be listed explicitly once there is a list.
			servers["https"].(route)["listener_wrappers"] = []route{wrapper, {"wrapper": "tls"}}
		}
		apps["tls"] = route{"automation": route{"policies": policies(in, httpsHosts)}}
	} else {
		servers["http"].(route)["automatic_https"] = route{"disable": true}
	}
	out, err := json.Marshal(route{"admin": route{"listen": in.AdminListen}, "apps": apps})
	if err != nil {
		return nil, err
	}
	return out, errors.Join(bad...)
}

// server orders routes the way Caddy needs them (it takes the first match):
// the panel first, exact hosts before wildcards, longer paths first.
func server(listen string, rs []placed, trusted []string, tail []json.RawMessage) route {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if (a.host == "") != (b.host == "") {
			return a.host == ""
		}
		if wa, wb := strings.HasPrefix(a.host, "*."), strings.HasPrefix(b.host, "*."); wa != wb {
			return wb
		}
		if len(a.path) != len(b.path) {
			return len(a.path) > len(b.path)
		}
		if a.host != b.host {
			return a.host < b.host
		}
		return a.path < b.path
	})
	routes := make([]any, len(rs))
	for i, r := range rs {
		routes[i] = r.r
	}
	for _, r := range tail {
		routes = append(routes, r) // after ours: the first match wins
	}
	s := route{"listen": []string{listen}, "routes": routes}
	if ranges := dedup(trusted); len(ranges) > 0 {
		s["trusted_proxies"] = route{"source": "static", "ranges": ranges}
	}
	return s
}

// customRoutes is proxy_custom as routes. "" is none; anything but a JSON
// array of objects is an error naming the setting.
func customRoutes(raw string) ([]json.RawMessage, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var rs []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &rs); err != nil {
		return nil, fmt.Errorf("proxy_custom: need a JSON array of Caddy routes: %w", err)
	}
	for i, r := range rs {
		var o map[string]any
		if json.Unmarshal(r, &o) != nil || o == nil {
			return nil, fmt.Errorf("proxy_custom: item %d is not a route object", i+1)
		}
	}
	return rs, nil
}

// customHosts are the host matchers of the custom routes, so their
// certificates follow the instance's account and DNS rules.
func customHosts(rs []json.RawMessage) (hosts []string) {
	for _, r := range rs {
		var m struct {
			Match []struct {
				Host []string `json:"host"`
			} `json:"match"`
		}
		_ = json.Unmarshal(r, &m)
		for _, x := range m.Match {
			hosts = append(hosts, x.Host...)
		}
	}
	return hosts
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range in {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

func match(host, path string, methods []string) []route {
	m := route{"host": []string{host}}
	if path != "" {
		m["path"] = []string{path, path + "/*"}
	}
	if len(methods) > 0 {
		m["method"] = methods
	}
	return []route{m}
}

// forceRoute bounces plain HTTP for this host and path onto HTTPS.
func forceRoute(host, path string) route {
	return route{
		"match":    match(host, path, nil),
		"terminal": true,
		"handle": []route{{
			"handler":     "static_response",
			"status_code": 308,
			"headers":     route{"Location": []string{"https://{http.request.host}{http.request.uri}"}},
		}},
	}
}

func domainRoute(d store.Domain, t TileRoute, expand Expand, tlsOn bool) (any, error) {
	if t.Expand != nil {
		expand = t.Expand
	}
	if d.RawCaddy != "" {
		return json.RawMessage(d.RawCaddy), nil
	}
	if d.RedirectTo != "" {
		// No auth, no headers: the redirect fires first anyway. A generated
		// one (a rename's old host, Auto) is a 308; a declared one a 301.
		// A generated row carries the target's HTTPS flag (the rename copies
		// it), so its Location's scheme follows it; a declared target is
		// whatever the admin named, https.
		code, scheme := 301, "https://"
		if d.Auto {
			code = 308
			if !tlsOn || !d.HTTPS {
				scheme = "http://"
			}
		}
		return route{
			"match":    match(d.Host, d.Path, nil),
			"terminal": true,
			"handle": []route{{
				"handler":     "static_response",
				"status_code": code,
				"headers":     route{"Location": []string{scheme + d.RedirectTo + "{http.request.uri}"}},
			}},
		}, nil
	}
	e, err := ExtrasOf(d)
	if err != nil {
		return nil, err
	}
	if err := checkProxyStrings(e); err != nil {
		return nil, err
	}
	var hs []route
	hs = append(hs, authHandlers(e, t, expand)...)
	if e.MaxBodyMB > 0 {
		hs = append(hs, route{"handler": "request_body", "max_size": e.MaxBodyMB << 20})
	}
	set := route{}
	if e.SecHeaders {
		set["Strict-Transport-Security"] = []string{"max-age=31536000; includeSubDomains"}
		set["X-Content-Type-Options"] = []string{"nosniff"}
		set["X-Frame-Options"] = []string{"DENY"}
	}
	for k, v := range e.Headers {
		set[k] = []string{v}
	}
	if len(set) > 0 {
		hs = append(hs, route{"handler": "headers", "response": route{"set": set}})
	}
	if e.StripPrefix && d.Path != "" {
		hs = append(hs, route{"handler": "rewrite", "strip_path_prefix": d.Path})
	}
	hs = append(hs, upstream(d, t, e))
	return route{"match": match(d.Host, d.Path, e.Methods), "terminal": true, "handle": hs}, nil
}

func upstream(d store.Domain, t TileRoute, e Extras) route {
	if len(t.Upstreams) == 0 {
		return route{"handler": "static_response", "status_code": 503, "body": "no running replica"}
	}
	names := append([]string(nil), t.Upstreams...)
	sort.Strings(names)
	ups := make([]route, len(names))
	for i, n := range names {
		ups[i] = route{"dial": n + ":" + strconv.Itoa(d.ContainerPort)}
	}
	rp := route{"handler": "reverse_proxy", "upstreams": ups}
	if t.HealthPath != "" {
		rp["health_checks"] = route{"active": route{"uri": t.HealthPath, "interval": "5s", "timeout": "3s"}}
	}
	if e.Websockets {
		rp["flush_interval"] = -1 // ponytail: Caddy proxies upgrades anyway; this only stops buffering
	}
	if tm := e.Timeouts; tm != nil {
		tr := route{"protocol": "http"}
		for k, s := range map[string]int{"dial_timeout": tm.Dial, "read_timeout": tm.Read, "write_timeout": tm.Write} {
			if s > 0 {
				tr[k] = strconv.Itoa(s) + "s"
			}
		}
		rp["transport"] = tr
	}
	return rp
}

// policies: wildcards on the DNS-01 issuer, hosts under a domain resource with
// its own email on that account, everything else (raw routes included) on
// the catch-all instance account.
func policies(in Install, hosts map[string]bool) []route {
	type key struct {
		email string
		dns   bool
	}
	groups := map[key][]string{}
	for h := range hosts {
		k := key{account(in.Accounts, h), strings.HasPrefix(h, "*.")}
		if k.email == "" {
			k.email = in.ACMEEmail
		}
		if k.dns && in.DNSProvider == "" || !k.dns && k.email == in.ACMEEmail {
			continue // the catch-all takes it
		}
		groups[k] = append(groups[k], h)
	}
	keys := slices.SortedFunc(maps.Keys(groups), func(a, b key) int {
		if a.dns != b.dns {
			if a.dns {
				return -1
			}
			return 1
		}
		return strings.Compare(a.email, b.email)
	})
	var out []route
	for _, k := range keys {
		iss := issuer(k.email)
		if k.dns {
			// The token stays in the proxy container's env, never in the
			// config stackrd pushes and Caddy autosaves.
			// ponytail: cloudflare is the one provider compiled into
			// `stackrd proxy`; another needs its module and its field.
			iss["challenges"] = route{"dns": route{"provider": route{
				"name":      in.DNSProvider,
				"api_token": "{env." + DNSTokenEnv + "}",
			}}}
		}
		sort.Strings(groups[k])
		out = append(out, route{"subjects": groups[k], "issuers": []route{iss}})
	}
	return append(out, route{"issuers": []route{issuer(in.ACMEEmail)}})
}

func issuer(email string) route {
	if email == "" {
		return route{"module": "acme"}
	}
	return route{"module": "acme", "email": email}
}

func account(accs []Account, host string) string {
	host = strings.TrimPrefix(host, "*.")
	best, email := -1, ""
	for _, a := range accs {
		base := strings.TrimPrefix(strings.ToLower(a.Host), "*.")
		if a.Email != "" && len(base) > best && (host == base || strings.HasSuffix(host, "."+base)) {
			best, email = len(base), a.Email
		}
	}
	return email
}

// Basic auth fails closed: an empty user or password, a ref that will not
// resolve, or a password bcrypt refuses (over 72 bytes) all become the lock,
// a real hash of random bytes nobody has. Never an open URL, never a
// sentinel Caddy might compare as plain text.
func credentials(a BasicAuth, expand Expand) (user, hash string) {
	pw := a.Password
	if expand != nil && pw != "" {
		var err error
		if pw, err = expand(pw); err != nil {
			slog.Warn("basic auth password did not resolve; route locked", "user", a.User, "err", err)
			pw = ""
		}
	}
	if a.User == "" || pw == "" || len(pw) > 72 {
		return "locked", lockHash()
	}
	return a.User, hashOf(pw)
}

// Cost 5: Caddy checks the hash on every request, assets included.
const cost = 5

// Both caches keep the config byte-stable: bcrypt salts differ per call, and
// a changed hash would make every unrelated save a reload.
var (
	lockHash = sync.OnceValue(func() string {
		b := make([]byte, 24)
		_, _ = rand.Read(b)
		h, _ := bcrypt.GenerateFromPassword(b, cost)
		return string(h)
	})
	hashes sync.Map // sha256(password) -> bcrypt hash
)

func hashOf(pw string) string {
	key := sha256.Sum256([]byte(pw))
	if h, ok := hashes.Load(key); ok {
		return h.(string)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), cost)
	if err != nil {
		return lockHash()
	}
	v, _ := hashes.LoadOrStore(key, string(h))
	return v.(string)
}
