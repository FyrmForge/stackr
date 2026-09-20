package service

import (
	"context"
	"crypto/tls"
	"strings"
	"time"

	"github.com/google/uuid"

	"gopkg.in/yaml.v3"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	svcproxy "github.com/FyrmForge/stackr/internal/stackrd/service/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// DomainService owns the hostnames a tile answers on.
//
// Every method takes the tile as well as the domain id and checks the two
// match. That is not defensive style, it is the fix for a live cross-tenant
// hole: the panel's HTTPS toggle and its live delete branch both acted on
// whatever `:domainID` named, and the only check above them proved write
// access to *the tile in the URL* — so an operator with a tile of their own
// could flip or delete any domain in the install by id.
type DomainService struct {
	store repo.Store
	px    *svcproxy.Service
	gate  *GateService
}

func NewDomainService(store repo.Store, px *svcproxy.Service, gate *GateService) *DomainService {
	return &DomainService{store: store, px: px, gate: gate}
}

// DomainSpec is a request to attach a hostname. The three pointers separate
// "not mentioned" from "set to false", which is what lets one rule serve a
// form checkbox, a JSON body and a config file.
type DomainSpec struct {
	Host        string
	Path        string
	Port        int // 0 inherits the tile's container port
	HTTPS       *bool
	ForceHTTPS  *bool
	RedirectTo  string
	Rule        string
	Priority    int
	Middlewares []string
}

// Attach validates a hostname against the tile and the rest of the install,
// and returns the row it would write.
//
// When staged is true nothing has been written: the caller records the
// returned row into the environment's pending set in whatever shape its
// surface stages. The validation has already happened either way, which is
// the point — a staged domain that could never apply is worse than a refusal.
func (s *DomainService) Attach(ctx context.Context, t *repo.Tile, spec DomainSpec, by Actor) (d *repo.Domain, staged bool, err error) {
	if t == nil {
		return nil, false, svcerr.ErrNotFound
	}
	stage, err := s.gateFor(ctx, t, by)
	if err != nil {
		return nil, false, err
	}
	row, err := s.plan(ctx, t, spec)
	if err != nil {
		return nil, false, err
	}
	if stage {
		return row, true, nil
	}
	if err := s.store.CreateDomain(ctx, row); err != nil {
		return nil, false, err
	}
	return row, false, s.px.SyncTile(ctx, t)
}

// gateFor asks the stack's gate, with one exception: a managed database
// instance. The config engine models domains for service tiles only
// (syncDomains/diffDomains return early on anything else), so a db-tile
// domain is invisible to plan and apply on every stack — there is nothing for
// the file to own and nothing that could be staged into a pending set. The
// panel has always written these straight through; this is what lets it keep
// doing so now that both surfaces come through here.
func (s *DomainService) gateFor(ctx context.Context, t *repo.Tile, by Actor) (bool, error) {
	if t.IsManaged() {
		return false, nil
	}
	return s.gate.Gate(ctx, t.StackID, GateFieldEdit, by.Surface())
}

// plan is every rule about a hostname, in one place. The API applied roughly
// a third of them.
func (s *DomainService) plan(ctx context.Context, t *repo.Tile, spec DomainSpec) (*repo.Domain, error) {
	host := strings.TrimSpace(spec.Host)
	if host == "" {
		return nil, invalid("host", "host required")
	}
	// Empty path normalises to "/", which is what the unique index stores.
	path := spec.Path
	if path == "" {
		path = "/"
	}
	// A managed instance only gets a route if its engine speaks HTTP at all.
	// The API skipped this and would happily route to a postgres port.
	if t.IsManaged() && !managedtiles.SpeaksHTTP(t.Engine) {
		return nil, invalid("host", "this engine does not speak HTTP, so it cannot have a hostname")
	}
	port := spec.Port
	if port == 0 {
		port = t.ContainerPort
	}
	if port == 0 {
		if spec.RedirectTo == "" {
			return nil, invalid("container_port", "container port required (set it on the app or the domain)")
		}
		// A redirect never proxies, so the target is unused — but 0 renders
		// as http://alias:0, which is not a URL anyone meant. The API left it
		// at zero and wrote exactly that into the route.
		port = 80
	}
	https := true
	if spec.HTTPS != nil {
		https = *spec.HTTPS
	}
	// Serving TLS implies the bounce unless the caller says otherwise. A bare
	// attach should produce a working HTTPS domain, because that is what
	// anyone adding a hostname wants; and "https off, force-redirect to https
	// on" — which is what the CLI's --no-https used to send — is incoherent.
	force := https
	if spec.ForceHTTPS != nil {
		force = *spec.ForceHTTPS
	}
	if strings.HasPrefix(host, "*.") && https {
		// A wildcard certificate can only be issued over DNS-01.
		if p, _ := s.store.GetSetting(ctx, "dns_provider"); p == "" {
			return nil, invalid("host", "wildcard HTTPS needs a DNS provider; configure it in Settings")
		}
	}
	// One host+path, one owner. A rule entry may share host+path with this
	// tile's own entries — its priority decides between them — but never with
	// another tile's, whose traffic it would take.
	if existing, _ := s.store.GetDomainByHostPath(ctx, host, path); existing != nil &&
		(existing.TileID != t.ID || (spec.Rule == "" && existing.Rule == "")) {
		return nil, svcerr.Conflictf("host + path already in use")
	}
	stack, err := s.store.GetStack(ctx, t.StackID)
	if err != nil || stack == nil {
		return nil, svcerr.ErrNotFound
	}
	// Anti-squat, the same rule org domain resources are under. Without it
	// the guard on the org page means nothing: anyone could claim the same
	// name one tile down. The API had no such guard.
	if err := CheckOrgSquat(ctx, s.store, host, stack.OrgID); err != nil {
		return nil, svcerr.Conflict{Msg: err.Error()}
	}
	// traefik disables a router naming a middleware it cannot find and says
	// so only in its own log, so a typo has to be refused here.
	// checkMiddlewares already names the field; re-wrapping it here printed
	// "middlewares: middlewares: no middleware nope".
	if err := s.checkMiddlewares(ctx, stack, spec.Middlewares); err != nil {
		return nil, err
	}
	return &repo.Domain{
		ID:            uuid.New().String(),
		TileID:        t.ID,
		Host:          host,
		Path:          path,
		ContainerPort: port,
		HTTPS:         https,
		ForceHTTPS:    force,
		RedirectTo:    strings.TrimSpace(spec.RedirectTo),
		Rule:          strings.TrimSpace(spec.Rule),
		Priority:      spec.Priority,
		Middlewares:   strings.Join(spec.Middlewares, "\n"),
		CreatedAt:     time.Now().UTC(),
	}, nil
}

// own resolves a domain id *and proves it belongs to this tile*. Every method
// below goes through it; see the type comment for what happened without it.
func (s *DomainService) own(ctx context.Context, t *repo.Tile, domainID string) (*repo.Domain, error) {
	if t == nil {
		return nil, svcerr.ErrNotFound
	}
	d, err := s.store.GetDomain(ctx, domainID)
	if err != nil || d == nil || d.TileID != t.ID {
		// Not "forbidden": a domain on someone else's tile must read as
		// missing, or the answer confirms the id exists.
		return nil, svcerr.ErrNotFound
	}
	return d, nil
}

// TLSPatch changes how a hostname is served. Every field is a pointer:
// nil means "not mentioned", which is what lets one method serve a panel
// button, a JSON PATCH and a config apply.
type TLSPatch struct {
	HTTPS      *bool
	ForceHTTPS *bool
	CertPEM    *string
	KeyPEM     *string
}

// SetTLS applies a TLS change to one of this tile's hostnames.
//
// The gate is the same field-edit gate every other setting takes. The panel's
// certificate handler had no gate at all, which meant a config-managed stack
// accepted a certificate its file would never know about; the panel's HTTPS
// toggle and the API disagreed about which gate applied.
//
// staged is reported for the caller to record, but a certificate is never
// staged: it is a secret, it is not in the file, and a pending set is not
// where a private key should sit waiting for someone to press Apply.
func (s *DomainService) SetTLS(ctx context.Context, t *repo.Tile, domainID string, p TLSPatch, by Actor) (d *repo.Domain, staged bool, err error) {
	stage, err := s.gateFor(ctx, t, by)
	if err != nil {
		return nil, false, err
	}
	d, err = s.own(ctx, t, domainID)
	if err != nil {
		return nil, false, err
	}
	if p.HTTPS != nil && *p.HTTPS && strings.HasPrefix(d.Host, "*.") {
		// A wildcard certificate can only be issued over DNS-01. The panel
		// checked this when attaching and not when toggling, so a wildcard
		// could be switched to HTTPS and then silently fail to get a cert.
		if prov, _ := s.store.GetSetting(ctx, "dns_provider"); prov == "" {
			return nil, false, invalid("https", "wildcard HTTPS needs a DNS provider; configure it in Settings")
		}
	}
	var cert, key string
	hasCert := p.CertPEM != nil || p.KeyPEM != nil
	if hasCert {
		if p.CertPEM != nil {
			cert = strings.TrimSpace(*p.CertPEM)
		}
		if p.KeyPEM != nil {
			key = strings.TrimSpace(*p.KeyPEM)
		}
		if cert != "" || key != "" {
			if _, err := tls.X509KeyPair([]byte(cert), []byte(key)); err != nil {
				return nil, false, invalid("cert_pem", "invalid certificate/key pair: "+err.Error())
			}
		}
	}
	if stage && !hasCert {
		return d, true, nil
	}
	if p.HTTPS != nil {
		if err := s.store.SetDomainHTTPS(ctx, d.ID, *p.HTTPS); err != nil {
			return nil, false, err
		}
		d.HTTPS = *p.HTTPS
	}
	if p.ForceHTTPS != nil {
		if err := s.store.SetDomainForceHTTPS(ctx, d.ID, *p.ForceHTTPS); err != nil {
			return nil, false, err
		}
		d.ForceHTTPS = *p.ForceHTTPS
	}
	if hasCert {
		if err := s.store.SetDomainCert(ctx, d.ID, cert, key); err != nil {
			return nil, false, err
		}
	}
	return d, false, s.px.SyncTile(ctx, t)
}

// ToggleHTTPS is the panel button: flip TLS, and keep the redirect in step
// with it. Leaving force_https behind would serve TLS while plain HTTP still
// answered unredirected, and would leave every later config plan carrying a
// change row that changes nothing.
func (s *DomainService) ToggleHTTPS(ctx context.Context, t *repo.Tile, domainID string, by Actor) (d *repo.Domain, on, staged bool, err error) {
	cur, err := s.own(ctx, t, domainID)
	if err != nil {
		return nil, false, false, err
	}
	on = !cur.HTTPS
	d, staged, err = s.SetTLS(ctx, t, domainID, TLSPatch{HTTPS: &on, ForceHTTPS: &on}, by)
	return d, on, staged, err
}

// Detach removes a hostname from a tile.
func (s *DomainService) Detach(ctx context.Context, t *repo.Tile, domainID string, by Actor) (d *repo.Domain, staged bool, err error) {
	stage, err := s.gateFor(ctx, t, by)
	if err != nil {
		return nil, false, err
	}
	d, err = s.own(ctx, t, domainID)
	if err != nil {
		return nil, false, err
	}
	if stage {
		return d, true, nil
	}
	if err := s.store.DeleteDomain(ctx, d.ID); err != nil {
		return nil, false, err
	}
	return d, false, s.px.SyncTile(ctx, t)
}

// middlewareNames reads the names out of a stack's proxy.middlewares blob.
// Deliberately only the names: this is the twin of stackconf.ParseMiddlewares,
// which returns the bodies too, and lives here because the service cannot
// import the config engine (envops imports the service, so that would close a
// cycle). Both read the same YAML shape, a map keyed by name.
func middlewareNames(blob string) map[string]bool {
	var m map[string]any
	_ = yaml.Unmarshal([]byte(blob), &m)
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// checkMiddlewares refuses a reference no stack file declares. A bare name is
// the tile's own stack's; stack/name is another stack's in the same org.
func (s *DomainService) checkMiddlewares(ctx context.Context, own *repo.Stack, refs []string) error {
	for _, ref := range refs {
		slug, name, cross := strings.Cut(ref, "/")
		st := own
		if cross {
			var err error
			if st, err = s.store.GetStackBySlug(ctx, own.OrgID, slug); err != nil || st == nil {
				return invalid("middlewares", "no stack "+slug+" in this org")
			}
		} else {
			name = slug
		}
		if !middlewareNames(st.ProxyMiddlewares)[name] {
			return invalid("middlewares",
				"no middleware "+ref+"; proxy.middlewares in the stack file declares them")
		}
	}
	return nil
}
