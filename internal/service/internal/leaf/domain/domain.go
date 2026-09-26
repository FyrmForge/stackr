// Package domain owns domains: the row, its Caddy route (the whole Caddy
// config is built here, from every tile's rows plus facts) and the tile's
// ingress network, created with the first domain and removed with the last.
// Pushing goes through Syncer so concurrent saves collapse into one run.
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Docker is the slice of the wrapper the ingress network needs.
type Docker interface {
	EnsureNetwork(ctx context.Context, name string, labels map[string]string) error
	RemoveNetwork(ctx context.Context, name string) error
	Connect(ctx context.Context, netName, containerID string, aliases []string) error
	Disconnect(ctx context.Context, netName, containerID string) error
	NetworkMembers(ctx context.Context, name string) ([]string, error)
	Inspect(ctx context.Context, id string) (docker.Detail, error)
}

type Leaf struct {
	domains store.DomainStore
	docker  Docker
	proxy   string // the proxy container; joins every ingress network
}

func New(domains store.DomainStore, d Docker, proxyContainer string) *Leaf {
	return &Leaf{domains: domains, docker: d, proxy: proxyContainer}
}

// Spec is one domain as the stack file, form or API gives it. Always the
// whole domain: Update replaces, it never patches.
type Spec struct {
	Host       string
	Path       string
	Port       int   // the resolved container port; the flow applies the tile's default
	HTTPS      *bool // nil = true
	ForceHTTPS *bool // nil = true
	RedirectTo string
	Auto       bool    // a generated host; renders exactly like a hand-attached one
	ResourceID *string // the domain resource that named an auto or apex host; nil for a literal
	Extras     Extras
	RawCaddy   string // admin-only: replaces the generated route verbatim
}

// On is the one nil-vs-false rule for HTTPS (B31): unset means on.
func On(b *bool) bool { return b == nil || *b }

// Extras are the named, safe-by-construction proxy knobs (stored as
// domains.proxy_json).
type Extras struct {
	BasicAuth   *BasicAuth        `json:"basic_auth,omitempty"`
	Websockets  bool              `json:"websockets,omitempty"`
	MaxBodyMB   int               `json:"max_body_mb,omitempty"`
	Timeouts    *Timeouts         `json:"timeouts,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Methods     []string          `json:"methods,omitempty"`
	StripPrefix bool              `json:"strip_prefix,omitempty"`
	SecHeaders  bool              `json:"sec_headers,omitempty"`
}

// BasicAuth: Password may be a ${{ }} ref, expanded at build time.
type BasicAuth struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// Timeouts are seconds on the upstream transport; 0 = Caddy's default.
type Timeouts struct {
	Dial  int `json:"dial,omitempty"`
	Read  int `json:"read,omitempty"`
	Write int `json:"write,omitempty"`
}

var (
	hostRe   = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	methodRe = regexp.MustCompile(`^[A-Z]+$`)
)

// Ingress is the tile's ingress network: its replicas and the proxy, nothing else.
func Ingress(tileID string) string { return "stackr-ingress-" + tileID }

func (l *Leaf) Get(ctx context.Context, id string) (store.Domain, error) {
	return l.domains.Get(ctx, id)
}

func (l *Leaf) List(ctx context.Context) ([]store.Domain, error) { return l.domains.List(ctx) }

func (l *Leaf) ListByTile(ctx context.Context, tileID string) ([]store.Domain, error) {
	return l.domains.ListByTile(ctx, tileID)
}

// Attach adds a domain to a tile. The first one opens the tile's ingress
// network and joins the proxy and the tile's running replicas (a fact) to it.
// dns01 says whether a DNS-01 provider is set; without one no wildcard can
// get a certificate, so a wildcard with HTTPS is refused.
func (l *Leaf) Attach(ctx context.Context, tileID string, s Spec, dns01 bool, replicas []string) (store.Domain, error) {
	d := store.Domain{ID: uuid.NewString(), TileID: tileID, CreatedAt: time.Now().UTC()}
	if err := l.fill(ctx, &d, s, dns01); err != nil {
		return d, err
	}
	have, err := l.domains.ListByTile(ctx, tileID)
	if err != nil {
		return d, err
	}
	d.Position = len(have)
	if len(have) == 0 {
		if err := l.OpenIngress(ctx, tileID, replicas); err != nil {
			return d, err
		}
	}
	return d, l.domains.Create(ctx, d)
}

// Update replaces a domain with s; tile and position stay.
func (l *Leaf) Update(ctx context.Context, d store.Domain, s Spec, dns01 bool) (store.Domain, error) {
	if err := l.fill(ctx, &d, s, dns01); err != nil {
		return d, err
	}
	return d, l.domains.Update(ctx, d)
}

// Detach removes a domain; the last one closes the tile's ingress network.
func (l *Leaf) Detach(ctx context.Context, d store.Domain) error {
	if err := l.domains.Delete(ctx, d.ID); err != nil {
		return err
	}
	left, err := l.domains.ListByTile(ctx, d.TileID)
	if err != nil || len(left) > 0 {
		return err
	}
	return l.CloseIngress(ctx, d.TileID)
}

// OpenIngress creates the tile's ingress network and connects the proxy and
// the given replicas. Idempotent.
func (l *Leaf) OpenIngress(ctx context.Context, tileID string, replicas []string) error {
	net := Ingress(tileID)
	if err := l.docker.EnsureNetwork(ctx, net, map[string]string{"stackr.tile": tileID}); err != nil {
		return err
	}
	for _, c := range append([]string{l.proxy}, replicas...) {
		if err := l.docker.Connect(ctx, net, c, nil); err != nil {
			return err
		}
	}
	return nil
}

// ProxyAddrs are the proxy container's IPs, one per network it has joined;
// none when it is not there.
func (l *Leaf) ProxyAddrs(ctx context.Context) ([]string, error) {
	d, err := l.docker.Inspect(ctx, l.proxy)
	if errors.Is(err, docker.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(d.Networks))
	for _, ip := range d.Networks {
		if ip != "" {
			out = append(out, ip)
		}
	}
	return out, nil
}

// CloseIngress disconnects every member of the tile's ingress network and
// removes it. Idempotent; the tile-delete flow calls it too, since the row
// cascade never reaches Detach.
func (l *Leaf) CloseIngress(ctx context.Context, tileID string) error {
	net := Ingress(tileID)
	members, err := l.docker.NetworkMembers(ctx, net)
	if err != nil {
		return err
	}
	for _, c := range members {
		if err := l.docker.Disconnect(ctx, net, c); err != nil {
			return err
		}
	}
	return l.docker.RemoveNetwork(ctx, net)
}

func (l *Leaf) fill(ctx context.Context, d *store.Domain, s Spec, dns01 bool) error {
	host := strings.ToLower(strings.TrimSpace(s.Host))
	if !hostRe.MatchString(host) {
		return errs.Invalidf("host", "%q is not a host name", s.Host)
	}
	path := strings.TrimSpace(s.Path)
	if path == "/" {
		path = ""
	}
	if path != "" && (!strings.HasPrefix(path, "/") || strings.ContainsAny(path, " *{}")) {
		return errs.Invalidf("path", "a path starts with / and has no spaces, * or braces")
	}
	https, force := On(s.HTTPS), On(s.ForceHTTPS)
	if https && strings.HasPrefix(host, "*.") && !dns01 {
		return errs.Invalidf("host", "a wildcard host needs a DNS-01 provider for its certificate")
	}
	redirect := strings.ToLower(strings.TrimSpace(s.RedirectTo))
	if redirect != "" && (!hostRe.MatchString(redirect) || strings.HasPrefix(redirect, "*.")) {
		return errs.Invalidf("redirect_to", "%q is not a host name", s.RedirectTo)
	}
	if redirect == "" && (s.Port < 1 || s.Port > 65535) {
		return errs.Invalidf("port", "the domain needs a container port")
	}
	if err := checkExtras(s.Extras); err != nil {
		return err
	}
	raw := strings.TrimSpace(s.RawCaddy)
	if raw != "" {
		var route map[string]any
		if json.Unmarshal([]byte(raw), &route) != nil {
			return errs.Invalidf("raw_caddy", "raw Caddy config must be one JSON route object")
		}
	}
	all, err := l.domains.List(ctx)
	if err != nil {
		return err
	}
	taken := slices.ContainsFunc(all, func(o store.Domain) bool {
		return o.ID != d.ID && o.Host == host && o.Path == path
	})
	if taken {
		return errs.Conflictf("%s%s is already attached to a tile", host, path)
	}
	extras, _ := json.Marshal(s.Extras)
	d.Host = host
	d.Path = path
	d.ContainerPort = s.Port
	d.HTTPS = https
	d.ForceHTTPS = force
	d.RedirectTo = redirect
	d.Auto = s.Auto
	d.ResourceID = s.ResourceID
	d.ProxyJSON = string(extras)
	d.RawCaddy = raw
	return nil
}

func checkExtras(e Extras) error {
	if e.BasicAuth != nil && e.BasicAuth.User == "" {
		return errs.Invalidf("proxy.basic_auth.user", "basic auth needs a user")
	}
	if e.MaxBodyMB < 0 {
		return errs.Invalidf("proxy.max_body_mb", "must not be negative")
	}
	if t := e.Timeouts; t != nil && (t.Dial < 0 || t.Read < 0 || t.Write < 0) {
		return errs.Invalidf("proxy.timeouts", "must not be negative")
	}
	for k := range e.Headers {
		if k == "" || strings.ContainsAny(k, " :\r\n") {
			return errs.Invalidf("proxy.headers", "%q is not a header name", k)
		}
	}
	for _, m := range e.Methods {
		if !methodRe.MatchString(m) {
			return errs.Invalidf("proxy.methods", "%q is not an HTTP method (upper case)", m)
		}
	}
	return nil
}

// ExtrasOf parses a row's proxy_json.
func ExtrasOf(d store.Domain) (Extras, error) {
	var e Extras
	if d.ProxyJSON == "" {
		return e, nil
	}
	if err := json.Unmarshal([]byte(d.ProxyJSON), &e); err != nil {
		return e, errors.New("domain " + d.Host + d.Path + ": proxy_json: " + err.Error())
	}
	return e, nil
}
