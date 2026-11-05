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

// Install is the install-wide half of the config: facts from settings and
// the domain resources' ACME accounts.
type Install struct {
	AdminListen    string // required on every config, or Caddy falls back to its own localhost
	ACMEEmail      string // the instance account
	Accounts       []Account
	Managed        []string // managed DNS domains: wildcards under them get DNS-01 through stackrd
	TLSOff         bool     // STACKR_TLS=off: plain HTTP only, no certificates
	IPv4Only       bool     // listen on 0.0.0.0 only, not the IPv6 wildcard
	TrustedProxies []string // the controller's front proxies; other nodes get none
	Controller     string   // the controller's node id; "" = every node keeps TrustedProxies
	PanelHost      string   // the panel's public host; skipped when a bare IP, localhost or ""
	PanelUpstream  string   // the panel's internal dial address, e.g. stackrd:8080
	// Custom is proxy_custom: a JSON array of Caddy HTTP route objects,
	// appended after every route of ours on the main server (https, or http
	// with TLS off). Additive, never a whole-config replace, so the panel
	// vhost and the tiles always survive it.
	Custom string
	// DefaultIngress is the node the panel route and any route without an
	// ingress node of its own follow. Only BuildNode and BuildAll read it.
	DefaultIngress string
	// IngressNodes are the nodes tagged ingress: BuildAll makes each one a
	// config even with nothing routed to it, so its probe answers.
	IngressNodes []string
	// Probe is the port-forward probe's path: on every host, over HTTP and
	// HTTPS, it answers ProbeID (BuildNode sets it to the node's id), so a
	// request from outside tells which node a NAT forwards to. "" = none.
	Probe, ProbeID string
	// AccessLog turns on JSON access logs to stdout (logger http.log.access)
	// for the tile-serving servers. By default the line carries no query
	// string, no headers but User-Agent, and the client address; the three
	// flags below put those parts back or take the address away.
	AccessLog                            bool
	AccessQuery, AccessIP, AccessHeaders bool
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
	// IngressNode is the node whose Caddy serves the rows ("" = the default
	// ingress node). Node is where the tile runs and VIP its address on the
	// env network; when the two nodes differ the upstream is VIP:port, not
	// the replica names.
	IngressNode, Node, VIP string
}

// NodeRoute is an external route with the ingress node that serves it ("" =
// the default ingress node).
type NodeRoute struct {
	Route
	IngressNode string
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
			// registry.<panel host> is the registry front, served by the panel.
			"match":    []route{{"host": []string{h, "registry." + h}}},
			"terminal": true,
			// X-Forwarded-For is set to the client Caddy resolved (its trusted
			// proxies applied), not appended: the panel trusts only Caddy, so a
			// front proxy's address would otherwise stand in for every user.
			"handle": []route{{"handler": "reverse_proxy", "upstreams": []route{{"dial": in.PanelUpstream}},
				"headers": route{"request": route{"set": route{"X-Forwarded-For": []string{"{http.vars.client_ip}"}}}}}},
		}
		if tlsOn {
			secure = append(secure, placed{"", "", serve})
			plain = append(plain, placed{"", "", forceRoute(h, "")}, placed{"", "", forceRoute("registry."+h, "")})
			httpsHosts[h], httpsHosts["registry."+h] = true, true
		} else {
			plain = append(plain, placed{"", "", serve})
		}
	}

	if in.Probe != "" && in.ProbeID != "" {
		r := route{"match": []route{{"path": []string{in.Probe}}}, "terminal": true,
			"handle": []route{{"handler": "static_response", "status_code": 200, "body": in.ProbeID}}}
		plain, secure = append(plain, placed{"", in.Probe, r}), append(secure, placed{"", in.Probe, r})
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

	bind := ""
	if in.IPv4Only {
		// tcp4: Go binds 0.0.0.0 dual-stack, so the network has to say IPv4.
		bind = "tcp4/0.0.0.0"
	}
	servers := route{"http": server(bind+":80", plain, in.TrustedProxies, httpTail, in)}
	// per_host: requests, status codes and latency per host on the admin
	// API's /metrics, which the panel's sampler turns into tile traffic.
	apps := route{"http": route{"servers": servers, "metrics": route{"per_host": true}}}
	if tlsOn {
		servers["https"] = server(bind+":443", secure, in.TrustedProxies, custom, in)
		servers["https"].(route)["automatic_https"] = route{"disable_redirects": true}
		if wrapper != nil {
			// The layer4 wrapper reads the SNI before TLS terminates; the
			// tls wrapper must be listed explicitly once there is a list.
			servers["https"].(route)["listener_wrappers"] = []route{wrapper, {"wrapper": "tls"}}
		}
		hosts := slices.Sorted(maps.Keys(httpsHosts))
		// The panel (and its registry front) never rides a wildcard: a
		// failing DNS-01 must not take its certificate down. Beside a
		// wildcard it is automated on its own certificate.
		panel := func(h string) bool { return in.PanelHost != "" && (h == in.PanelHost || h == "registry."+in.PanelHost) }
		wild := WildcardSubjects(in.Managed, slices.DeleteFunc(slices.Clone(hosts), panel))
		tlsApp := route{"automation": route{"policies": policies(in, hosts, wild)}}
		if len(wild) > 0 {
			// Managed, so hosts a wildcard covers get no certificate of their own.
			subj := make([]string, len(wild))
			for i, w := range wild {
				subj[i] = w.Subject
			}
			for _, h := range hosts {
				if panel(h) && ManagedOf(h, in.Managed) != "" {
					subj = append(subj, h)
				}
			}
			tlsApp["certificates"] = route{"automate": subj}
		}
		apps["tls"] = tlsApp
	} else {
		servers["http"].(route)["automatic_https"] = route{"disable": true}
	}
	cfg := route{"admin": route{"listen": in.AdminListen}, "apps": apps}
	if in.AccessLog {
		cfg["logging"] = accessLogging(in)
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return out, errors.Join(bad...)
}

// BuildNode is Build for one ingress node: only the tiles and routes that
// node serves, and the panel route only on the default ingress node.
func BuildNode(node string, in Install, tiles []TileRoute, routes []NodeRoute, expand Expand) (json.RawMessage, error) {
	at := func(n string) bool { return n == node || n == "" && in.DefaultIngress == node }
	if in.DefaultIngress != node {
		in.PanelHost = ""
	}
	if in.Controller != "" && in.Controller != node {
		in.TrustedProxies = nil // the front proxies are the controller's
	}
	in.ProbeID = node
	var ts []TileRoute
	for _, t := range tiles {
		if at(t.IngressNode) {
			if t.IngressNode == "" {
				t.IngressNode = in.DefaultIngress
			}
			ts = append(ts, t)
		}
	}
	var rs []Route
	for _, r := range routes {
		if at(r.IngressNode) {
			rs = append(rs, r.Route)
		}
	}
	return Build(in, ts, rs, expand)
}

// BuildAll is one config per ingress node: every node a tile or route names,
// every node tagged ingress, plus the default ingress node. Errors are
// joined; configs that built are still returned.
func BuildAll(in Install, tiles []TileRoute, routes []NodeRoute, expand Expand) (map[string]json.RawMessage, error) {
	nodes := map[string]bool{in.DefaultIngress: true}
	for _, n := range in.IngressNodes {
		nodes[n] = true
	}
	for _, t := range tiles {
		if t.IngressNode != "" {
			nodes[t.IngressNode] = true
		}
	}
	for _, r := range routes {
		if r.IngressNode != "" {
			nodes[r.IngressNode] = true
		}
	}
	out := map[string]json.RawMessage{}
	var errs []error
	for n := range nodes {
		cfg, err := BuildNode(n, in, tiles, routes, expand)
		if cfg != nil {
			out[n] = cfg
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("ingress node %q: %w", n, err))
		}
	}
	return out, errors.Join(errs...)
}

// server orders routes the way Caddy needs them (it takes the first match):
// the panel first, exact hosts before wildcards, longer paths first.
func server(listen string, rs []placed, trusted []string, tail []json.RawMessage, in Install) route {
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
	var routes []any
	if in.AccessLog && !in.AccessHeaders {
		// The filter below drops every request header; User-Agent comes back
		// as its own field.
		routes = append(routes, route{"handle": []route{{"handler": "log_append", "key": "user_agent",
			"value": "{http.request.header.User-Agent}", "early": true}}})
	}
	for _, r := range rs {
		routes = append(routes, r.r)
	}
	for _, r := range tail {
		routes = append(routes, r) // after ours: the first match wins
	}
	// A host nothing routes is a 404, not Caddy's empty 200. ACME HTTP
	// challenges are answered before the routes run.
	routes = append(routes, route{"handle": []route{{"handler": "static_response", "status_code": 404}}})
	s := route{"listen": []string{listen}, "routes": routes}
	if in.AccessLog {
		s["logs"] = route{} // switches the server's access log on
	}
	if ranges := dedup(ExpandProxies(trusted, false)); len(ranges) > 0 {
		s["trusted_proxies"] = route{"source": "static", "ranges": ranges}
	}
	return s
}

// accessLogging is the logging config: the access log to stdout as JSON,
// through a filter that strips what the Install does not keep. Cookie,
// Authorization and the like are headers, so they go with them.
func accessLogging(in Install) route {
	fields := route{}
	if !in.AccessQuery {
		fields["request>uri"] = route{"filter": "regexp", "regexp": `\?.*$`, "value": ""}
	}
	if !in.AccessHeaders {
		fields["request>headers"] = route{"filter": "delete"}
		fields["resp_headers"] = route{"filter": "delete"}
	}
	if !in.AccessIP {
		for _, k := range []string{"remote_ip", "remote_port", "client_ip"} {
			fields["request>"+k] = route{"filter": "delete"}
		}
	}
	return route{"logs": route{"default": route{"exclude": []string{"http.log.access"}}, "access": route{
		"writer":  route{"output": "stdout"},
		"encoder": route{"format": "filter", "wrap": route{"format": "json"}, "fields": fields},
		"include": []string{"http.log.access"},
	}}}
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
	if t.VIP != "" {
		// A tile on a worker: its replicas join no ingress network, so even
		// that worker's own Caddy (host network) reaches it by VIP only.
		names = []string{t.VIP}
	}
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

// Wildcard is a wildcard certificate subject and the managed domain whose
// DNS answers its DNS-01 challenge.
type Wildcard struct{ Subject, Domain string }

// WildcardSubjects is the wildcards the hosts in managed domains need, one
// per level in use: a * covers one label, so each host is covered by
// *.<its parent> when that parent is the managed domain or under it. Auto
// names (tile.stack.org.<root>) share *.stack.org.<root>; typed one-level
// names share *.<domain>. An apex, a host in no managed domain and a
// wildcard host get none (the apex keeps its own certificate). hosts are
// one node's, so each ingress node issues only the wildcards it serves.
func WildcardSubjects(managed, hosts []string) []Wildcard {
	var out []Wildcard
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSuffix(h, "."))
		if strings.HasPrefix(h, "*.") {
			continue
		}
		_, parent, ok := strings.Cut(h, ".")
		if !ok {
			continue
		}
		if m := ManagedOf(parent, managed); m != "" {
			if w := (Wildcard{Subject: "*." + parent, Domain: m}); !slices.Contains(out, w) {
				out = append(out, w)
			}
		}
	}
	slices.SortFunc(out, func(a, b Wildcard) int { return strings.Compare(a.Subject, b.Subject) })
	return out
}

// ManagedOf is the managed domain host is at or under ("" = none). A
// wildcard host counts as its base.
func ManagedOf(host string, managed []string) string {
	host = strings.TrimPrefix(strings.ToLower(host), "*.")
	best := ""
	for _, m := range managed {
		if (host == m || strings.HasSuffix(host, "."+m)) && len(m) > len(best) {
			best = m
		}
	}
	return best
}

// policies: wildcards under a managed domain (wild, plus wildcard hosts in
// use) on DNS-01 through stackrd, hosts under a domain resource with its own
// email on that account, everything else (raw routes included) on the
// catch-all instance account. A wildcard under no managed domain cannot be
// issued; it stays on the catch-all.
func policies(in Install, hosts []string, wild []Wildcard) []route {
	type key struct {
		email string
		dns   bool
	}
	groups := map[key][]string{}
	add := func(h string, dns bool) {
		k := key{account(in.Accounts, h), dns}
		if k.email == "" {
			k.email = in.ACMEEmail
		}
		if !k.dns && k.email == in.ACMEEmail || slices.Contains(groups[k], h) {
			return // the catch-all takes it, or it is there already
		}
		groups[k] = append(groups[k], h)
	}
	for _, w := range wild {
		add(w.Subject, true)
	}
	for _, h := range hosts {
		add(h, strings.HasPrefix(h, "*.") && ManagedOf(h, in.Managed) != "")
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
			// No key in the config: the module asks stackrd, which holds
			// the DNS connectors.
			iss["challenges"] = route{"dns": route{"provider": route{"name": "stackr"}}}
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

// CloudflareKeyword in the trusted_proxies setting stands for CloudflareV4
// and CloudflareV6.
const CloudflareKeyword = "cloudflare"

// Cloudflare's published edge ranges (cloudflare.com/ips), baked in.
// ponytail: static; refresh by hand when Cloudflare announces a change.
var (
	CloudflareV4 = []string{
		"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
		"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
		"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
		"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	}
	CloudflareV6 = []string{
		"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
		"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
	}
)

// ExpandProxies replaces the cloudflare keyword with its ranges (IPv4
// only when v4Only: the firewall filter has no IPv6).
func ExpandProxies(in []string, v4Only bool) []string {
	var out []string
	for _, r := range in {
		if !strings.EqualFold(r, CloudflareKeyword) {
			out = append(out, r)
			continue
		}
		out = append(out, CloudflareV4...)
		if !v4Only {
			out = append(out, CloudflareV6...)
		}
	}
	return out
}
