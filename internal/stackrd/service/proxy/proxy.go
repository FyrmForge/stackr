// Package proxy owns every write to the Traefik configuration. Before it
// existed there were 44 write sites spread over the web panel, the API,
// config-as-code and boot, with seven different rule sets around them, and the
// two surfaces disagreed about which writes happen at all: a tile settings
// save rewrote the route from the panel and did not from the API, so
// basic_auth, security headers and a traefik override set over the API did
// nothing until the next redeploy.
//
// Nothing above this package holds a *infraproxy.Proxy.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	yaml "go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/netaddr"
	"github.com/FyrmForge/stackr/internal/stackrd/config/envutil"
	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	infraproxy "github.com/FyrmForge/stackr/internal/stackrd/infra/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// invalid is a refusal caused by what the caller sent. The vocabulary is
// svcerr's (D4) so that one mapper at the edge answers for every service;
// this package used to own its own ErrInvalid/ErrNoEntry pair, which is how
// two error mappers came to exist for two handlers.
func invalid(format string, a ...any) error { return svcerr.Invalidf("", format, a...) }

// Service is the only thing that writes Traefik config.
type Service struct {
	store repo.Store
	px    *infraproxy.Proxy

	// The managed registry's route needs the registry service itself ensured
	// first, because the alias Traefik dials lives in that service's spec.
	rt *runtime.Runtime
	// regs owns the registry row EnsureManaged writes. This package is under
	// service/ but is its own package, so it takes the interface like the
	// infra callers do rather than importing the parent.
	regs         registry.Registries
	signer       *registry.Signer
	dataDir      string
	registryPort string
	baseURL      string

	// EnsureTraefik rewrites the static config and restarts the container.
	// Nothing used to stop two saves racing two goroutines through the same
	// rewrite-and-restart. One runs at a time; callers that arrive while one
	// is in flight coalesce into a single follow-up run, because they all want
	// the same thing: traefik running on the newest config.
	mu      sync.Mutex
	running bool
	queued  bool
	// runMu is held for the duration of one run, so the boot path and the
	// background loop cannot both be inside px.EnsureTraefik at once.
	runMu sync.Mutex
	// ensure is px.EnsureTraefik, indirected so the coalescing can be tested
	// without a docker daemon.
	ensure func(context.Context) error
}

// New builds the service. rt, signer and the three strings are only needed by
// SetRegistryDomain; everything else works without them.
func New(store repo.Store, px *infraproxy.Proxy, regs registry.Registries, rt *runtime.Runtime, signer *registry.Signer, dataDir, registryPort, baseURL string) *Service {
	s := &Service{store: store, px: px, regs: regs, rt: rt, signer: signer,
		dataDir: dataDir, registryPort: registryPort, baseURL: baseURL}
	if px != nil {
		s.ensure = px.EnsureTraefik
	}
	return s
}

// available reports whether there is a proxy to write to. Nil in tests and in
// the API router's unit tests, which is why every method checks.
func (s *Service) available() bool { return s != nil && s.px != nil }

// --- tile routes ---

// SyncTile rewrites one tile's route file from its current domains. Every
// caller used to be a hand-written ListDomainsByTile + WriteApp pair, and the
// API's settings patch was missing its copy.
//
// Kinds that never serve HTTP are skipped here rather than at each call site,
// which is where the two copies of that check were.
func (s *Service) SyncTile(ctx context.Context, t *repo.Tile) error {
	if !s.available() || t == nil || t.Kind == "cron" || t.Kind == "function" {
		return nil
	}
	domains, err := s.store.ListDomainsByTile(ctx, t.ID)
	if err != nil {
		return err
	}
	return s.px.WriteApp(t, domains)
}

// DropTile removes a tile's route file. It returns nothing: the delete that
// preceded it has already torn the service down, and a route file that will
// not unlink must not be what fails it. Resync prunes orphans anyway.
func (s *Service) DropTile(tileID string) {
	if !s.available() {
		return
	}
	if err := s.px.RemoveApp(tileID); err != nil {
		slog.Error("proxy route not removed", "tile", tileID, "error", err)
	}
}

// SyncStackMiddlewares rewrites a stack's shared middleware file.
func (s *Service) SyncStackMiddlewares(st *repo.Stack) error {
	if !s.available() {
		return nil
	}
	return s.px.WriteStackMiddlewares(st)
}

// Resync regenerates every dynamic file from the database and prunes orphans.
//
// It logs its own failure, so no caller has to decide a policy, and it returns
// the error as well for the one caller that reports rather than serves: a
// config apply folds it into the plan's own output. A handler calls it as a
// statement and ignores the result — the write it follows has already
// succeeded, so failing the request would report the wrong thing.
func (s *Service) Resync(ctx context.Context) error {
	if !s.available() {
		return nil
	}
	err := s.px.Resync(ctx)
	if err != nil {
		slog.Error("proxy resync failed", "error", err)
	}
	return err
}

// AccessLog is the tile's most recent proxied requests, newest first.
func (s *Service) AccessLog(tileID string, limit int) []infraproxy.AccessEntry {
	if !s.available() {
		return nil
	}
	return s.px.AccessLog(tileID, limit)
}

// --- traefik static config ---

// EnsureTraefik rewrites the static config and restarts Traefik, in the
// background. Every caller but boot wants this: the restart takes seconds and
// a client that disconnects must not cancel it half way, which is what the
// ACME patch used to do by running it on the request context.
func (s *Service) EnsureTraefik() {
	if s == nil || s.ensure == nil {
		return
	}
	s.mu.Lock()
	if s.running {
		// Someone is already rewriting. Their run may have read the settings
		// before ours landed, so ask for one more pass after it, but only one
		// however many callers pile up.
		s.queued = true
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()
	go s.ensureLoop()
}

func (s *Service) ensureLoop() {
	for {
		s.runMu.Lock()
		err := s.ensure(context.Background())
		s.runMu.Unlock()
		if err != nil {
			slog.Error("traefik reconfigure failed", "error", err)
		}
		s.mu.Lock()
		if !s.queued {
			s.running = false
			s.mu.Unlock()
			return
		}
		s.queued = false
		s.mu.Unlock()
	}
}

// EnsureTraefikNow is the boot path: it returns the error rather than logging
// it, because nothing is serving yet and a Traefik that will not come up is
// worth reporting.
//
// It still goes through the same slot as every other caller. It runs in a
// goroutine at boot while the HTTP server is already accepting, so an operator
// saving proxy settings during the image pull is a real window: taking the
// slot without checking used to run two reconfigures at once, and clearing
// queued on the way out used to drop that operator's save on the floor — the
// panel said "saved" and the setting never reached Traefik until the next
// restart.
func (s *Service) EnsureTraefikNow(ctx context.Context) error {
	if s == nil || s.ensure == nil {
		return nil
	}
	s.mu.Lock()
	// A loop already in flight owns the running flag and will drain the queue
	// itself; we only have to not run concurrently with it.
	owned := !s.running
	s.running = true
	s.mu.Unlock()

	s.runMu.Lock()
	err := s.ensure(ctx)
	s.runMu.Unlock()

	s.mu.Lock()
	switch {
	case s.queued && owned:
		// Somebody saved while we were running. Hand the follow-up to the
		// loop rather than discarding it; running stays true for it.
		s.queued = false
		s.mu.Unlock()
		go s.ensureLoop()
	case owned:
		s.running = false
		s.mu.Unlock()
	default:
		// A loop is live and owns both flags. Leaving queued alone is
		// deliberate: the loop drains it on its own next pass.
		s.mu.Unlock()
	}
	return err
}

// CurrentStatic is the traefik.yml currently on disk.
func (s *Service) CurrentStatic() string {
	if !s.available() {
		return ""
	}
	return s.px.CurrentStatic()
}

// StaticOverride is the operator's verbatim traefik.yml, empty when the
// generated default is in use.
func (s *Service) StaticOverride(ctx context.Context) string {
	v, _ := s.store.GetSetting(ctx, settings.KeyStaticOverride)
	return v
}

// SetStaticOverride stores a verbatim traefik.yml. Empty clears it.
func (s *Service) SetStaticOverride(ctx context.Context, body string) error {
	if strings.TrimSpace(body) == "" {
		body = ""
	} else if err := validYAML(body); err != nil {
		return invalid("not valid YAML: %v", err)
	}
	if err := s.store.SetSetting(ctx, settings.KeyStaticOverride, body); err != nil {
		return err
	}
	s.EnsureTraefik()
	return nil
}

// DNS is the configured DNS-01 provider and its credential env lines; an
// empty provider means wildcard certificates are off.
//
// The render path in infra/proxy used to read both keys itself. Same two
// keys, typed a second time, with nothing saying they were the same two —
// and config/stackconf typed dns_provider a third time to decide whether to
// let a wildcard domain through at all. A provider set here and misspelled
// there is a panel that accepts the domain and a proxy that never gets a
// certificate for it.
func (s *Service) DNS(ctx context.Context) (provider string, env []string) {
	provider, _ = s.store.GetSetting(ctx, settings.KeyDNSProvider)
	raw, _ := s.store.GetSetting(ctx, settings.KeyDNSEnv)
	return provider, envutil.Lines(raw)
}

// SetDNS stores the DNS-01 provider and its credentials, for wildcard certs.
func (s *Service) SetDNS(ctx context.Context, provider, env string) error {
	if err := s.store.SetSetting(ctx, settings.KeyDNSProvider, strings.TrimSpace(provider)); err != nil {
		return err
	}
	if err := s.store.SetSetting(ctx, settings.KeyDNSEnv, env); err != nil {
		return err
	}
	s.EnsureTraefik()
	return nil
}

// SetResourceACME points one domain resource at its own Let's Encrypt account.
// The account lives in the static config, so the resolver set has to be
// rewritten before the next certificate is asked for.
func (s *Service) SetResourceACME(ctx context.Context, resourceID, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if err := s.store.SetDomainResourceACME(ctx, resourceID, email); err != nil {
		return err
	}
	s.EnsureTraefik()
	return nil
}

// TrustedProxies is the stored CIDR list and the Cloudflare toggle.
func (s *Service) TrustedProxies(ctx context.Context) (raw string, trustCF bool) {
	raw, _ = s.store.GetSetting(ctx, settings.KeyTrustedProxies)
	v, _ := s.store.GetSetting(ctx, settings.KeyTrustCF)
	return raw, v == "1"
}

// CloudflareCIDRs is the cached copy of Cloudflare's published edge ranges,
// "" when the toggle has never been on.
func (s *Service) CloudflareCIDRs(ctx context.Context) string {
	v, _ := s.store.GetSetting(ctx, settings.KeyCFCIDRs)
	return v
}

// SetCloudflareCIDRs replaces the cache. Sorted here rather than at the call
// site: Cloudflare reordering its own list must not come back as a changed
// static config and a recreated Traefik container.
func (s *Service) SetCloudflareCIDRs(ctx context.Context, cidrs []string) error {
	sort.Strings(cidrs)
	return s.store.SetSetting(ctx, settings.KeyCFCIDRs, strings.Join(cidrs, "\n"))
}

// SetTrustedProxies replaces the CIDRs Traefik takes X-Forwarded-For from.
//
// A bad line refuses the whole save. The installer seed used to skip one with
// a warning, which is worse than it sounds: a dropped CIDR is a trusted proxy
// that is not trusted, and it shows up later as every client IP being the
// proxy's — in the rate limiter, in the access log and in the audit trail.
func (s *Service) SetTrustedProxies(ctx context.Context, raw string, trustCF bool) error {
	lines, err := ParseTrustedList(raw)
	if err != nil {
		return err
	}
	if trustCF && s.available() {
		if err := s.px.RefreshCloudflare(ctx); err != nil {
			if cached, _ := s.store.GetSetting(ctx, settings.KeyCFCIDRs); strings.TrimSpace(cached) == "" {
				slog.Warn("cloudflare ranges fetch failed", "error", err)
				return fmt.Errorf("could not reach Cloudflare: %w", err)
			}
			slog.Warn("cloudflare ranges fetch failed, using cache", "error", err)
		}
	}
	flag := ""
	if trustCF {
		flag = "1"
	}
	if err := s.store.SetSetting(ctx, settings.KeyTrustedProxies, strings.Join(lines, "\n")); err != nil {
		return err
	}
	if err := s.store.SetSetting(ctx, settings.KeyTrustCF, flag); err != nil {
		return err
	}
	s.EnsureTraefik()
	return nil
}

// ParseTrustedList validates a newline-separated CIDR list, one policy for the
// panel, the API and the installer seed. Blank lines are skipped; a line that
// will not parse fails the list.
func ParseTrustedList(raw string) ([]string, error) {
	var out []string
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == ',' }) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cidr, err := netaddr.ParseTrusted(line)
		if err != nil {
			return nil, svcerr.Invalid{Msg: err.Error()}
		}
		out = append(out, cidr)
	}
	return out, nil
}

// --- custom dynamic entries ---

// Entries is the operator's named dynamic-config entries, name -> raw yaml.
func (s *Service) Entries(ctx context.Context) map[string]string {
	raw, _ := s.store.GetSetting(ctx, settings.KeyCustomDynamic)
	out := map[string]string{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

// SetEntry writes (or replaces) one named entry. name is slugified; an empty
// name or invalid YAML refuses the save.
func (s *Service) SetEntry(ctx context.Context, name, body string) (string, error) {
	name = repo.Slugify(name)
	if name == "" {
		return "", invalid("entry name required")
	}
	if err := validYAML(body); err != nil {
		return "", invalid("not valid YAML: %v", err)
	}
	entries := s.Entries(ctx)
	entries[name] = body
	return name, s.saveEntries(ctx, entries)
}

// DeleteEntry removes one named entry.
func (s *Service) DeleteEntry(ctx context.Context, name string) error {
	entries := s.Entries(ctx)
	if _, ok := entries[name]; !ok {
		return svcerr.ErrNotFound
	}
	delete(entries, name)
	return s.saveEntries(ctx, entries)
}

func (s *Service) saveEntries(ctx context.Context, entries map[string]string) error {
	b, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	if err := s.store.SetSetting(ctx, settings.KeyCustomDynamic, string(b)); err != nil {
		return err
	}
	if !s.available() {
		return nil
	}
	return s.px.SyncCustomDynamic(entries)
}

// --- managed registry route ---

// SetRegistryDomain points the managed registry at a TLS hostname and writes
// its Traefik route. The registry service is ensured first because the
// "registry" network alias Traefik dials lives in that service's spec, and the
// API path used to skip it: Traefik then routed to an alias nothing carried.
func (s *Service) SetRegistryDomain(ctx context.Context, reg *repo.Registry, domain string) error {
	reg.Domain = strings.TrimSpace(domain)
	if err := s.store.UpdateRegistry(ctx, reg); err != nil {
		return err
	}
	if reg.Domain != "" {
		if _, err := registry.EnsureManaged(ctx, s.store, s.regs, s.rt, s.signer, s.dataDir, s.registryPort, s.baseURL); err != nil {
			return err
		}
	}
	if !s.available() {
		return nil
	}
	return s.px.WriteRegistry(reg.Domain)
}

func validYAML(s string) error {
	var v map[string]any
	return yaml.Unmarshal([]byte(s), &v)
}
