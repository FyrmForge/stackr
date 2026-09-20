// Domain resources are the hostnames stackr may generate names under, and
// the rules for what a hostname is allowed to be. They moved here from
// config/envops because the tile service validates a custom domain against
// them, and envops imports the service — the rules had to sit below both.
package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	svcproxy "github.com/FyrmForge/stackr/internal/stackrd/service/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/google/uuid"
)

// ValidateResourceHost checks a domain-resource host: non-empty, lowercase,
// a bare hostname (no scheme, slash, port or spaces), and a name that can
// actually be a domain. One rule shared by the panel forms and the API so
// every surface rejects the same inputs.
//
// The last part is installspec's, the installer's own grammar: this used to
// accept localhost, a bare IP and a single label, all of which the installer
// on the same box refuses when asked for the root domain.
func ValidateResourceHost(host string) error {
	if host == "" {
		return fmt.Errorf("host required")
	}
	if host != strings.ToLower(host) {
		return fmt.Errorf("host must be lowercase")
	}
	if strings.ContainsAny(host, "/: ") || strings.Contains(host, "//") {
		return fmt.Errorf("host must be a bare hostname; no scheme, slash or port")
	}
	if _, err := installspec.CheckRoot(host); err != nil {
		return err
	}
	return nil
}

// HostTaken reports whether another resource already owns host (hosts are
// globally unique, the DB index enforces it, this gives a friendly error).
func HostTaken(all []repo.DomainResource, host string) bool {
	for _, r := range all {
		if r.Host == host {
			return true
		}
	}
	return false
}

// CheckOrgSquat rejects a hostname whose first label is another organization's
// slug. Generated hostnames nest under an org's own domains, so a host like
// orgb.example.com claimed by org A can shadow or impersonate org B's
// addresses. Every path that accepts a hostname goes through here: org domain
// resources, tile-level custom domains and config-as-code domain blocks.
func CheckOrgSquat(ctx context.Context, store repo.Store, host, ownOrgID string) error {
	label, _, ok := strings.Cut(strings.TrimPrefix(host, "*."), ".")
	if !ok || label == "" {
		return nil
	}
	other, err := store.GetOrgBySlug(ctx, label)
	if err != nil {
		// A guard against impersonation may not fail open on a store blip.
		return fmt.Errorf("could not check that hostname against organization slugs: %w", err)
	}
	if other == nil || other.ID == ownOrgID {
		return nil
	}
	return fmt.Errorf("that hostname starts with another organization's slug")
}

// AutoHost is the generated hostname for a tile under a domain resource:
// dotted segments truncated at the owning level (a stack-owned base already
// implies the stack, so its segment is dropped), the default env's slug
// omitted unless the resource opts in.
func AutoHost(res repo.DomainResource, orgSlug, stackSlug, envSlug, tileSlug string, isDefaultEnv bool) string {
	segs := []string{tileSlug}
	if !isDefaultEnv || res.IncludeEnvOnDefault {
		segs = append(segs, envSlug)
	}
	switch res.Level {
	case "stack":
	case "org":
		segs = append(segs, stackSlug)
	default: // instance
		segs = append(segs, stackSlug, orgSlug)
	}
	return strings.Join(segs, ".") + "." + res.Host
}

// VisibleDomainResources filters to what a stack may claim under, its own,
// its org's, the instance's, nearest level first, then a declared resource
// ahead of an undeclared one, oldest first within that.
//
// Declared wins because the wizard hands an org with no domain of its own a
// default under the server's hostname, so its stacks get names at all. That
// row is older than anything a config file declares later, and oldest-first
// alone would leave the default beating the domain the file actually asked
// for.
func VisibleDomainResources(all []repo.DomainResource, stackID, orgID string) []repo.DomainResource {
	rank := func(r repo.DomainResource) int {
		lvl := 2
		switch r.Level {
		case "stack":
			lvl = 0
		case "org":
			lvl = 1
		}
		if r.Declared {
			return lvl * 2
		}
		return lvl*2 + 1
	}
	var out []repo.DomainResource
	for _, r := range all {
		switch r.Level {
		case "stack":
			if r.OwnerID == stackID {
				out = append(out, r)
			}
		case "org":
			if r.OwnerID == orgID {
				out = append(out, r)
			}
		default: // instance, single server today; every stack sees it
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

// DomainResourceService owns the hostnames stackr may generate names under.
//
// A resource is a claim on a piece of DNS, so the rules around creating one
// are about who else might already own it — which is why the squat check
// belongs here rather than only on the org page, where it used to live. The
// tenancy question ("may this caller write at this level") stays at the edge:
// it is answered from the request's identity, not from the row.
type DomainResourceService struct {
	store repo.Store
	px    *svcproxy.Service
}

func NewDomainResourceService(store repo.Store, px *svcproxy.Service) *DomainResourceService {
	return &DomainResourceService{store: store, px: px}
}

// ResourceOpts are the settings a resource carries beyond its host.
type ResourceOpts struct {
	// IncludeEnvOnDefault keeps the environment segment in generated names
	// even for the default environment.
	IncludeEnvOnDefault bool
	// ACMEEmail is the Let's Encrypt account this host's certificates are
	// issued on, empty for the install-wide account. The API accepted this
	// key and threw it away, and the panel had no field for it at all.
	ACMEEmail string
}

// Create adds a domain resource at a level, for an owner the caller has
// already proved they may write to.
//
// level accepts "node" as a spelling of "instance" so a caller that thinks in
// nodes does not have to learn stackr's word for it; only one value is stored.
func (s *DomainResourceService) Create(ctx context.Context, level, ownerID, host string, opts ResourceOpts) (*repo.DomainResource, error) {
	level, ownerID = canonLevel(level, ownerID)
	host = strings.TrimSpace(strings.ToLower(host))
	if err := ValidateResourceHost(host); err != nil {
		return nil, invalid("host", err.Error())
	}
	all, err := s.store.ListDomainResources(ctx)
	if err != nil {
		return nil, err
	}
	if HostTaken(all, host) {
		return nil, svcerr.Conflictf("that host is already a domain resource")
	}
	if err := s.checkOwner(ctx, level, ownerID, host); err != nil {
		return nil, err
	}
	r := &repo.DomainResource{
		ID: uuid.New().String(), Level: level, OwnerID: ownerID, Host: host,
		IncludeEnvOnDefault: opts.IncludeEnvOnDefault,
		ACMEEmail:           strings.ToLower(strings.TrimSpace(opts.ACMEEmail)),
		CreatedAt:           time.Now().UTC(),
	}
	if err := s.store.CreateDomainResource(ctx, r); err != nil {
		return nil, err
	}
	if r.ACMEEmail != "" {
		// The account lives in Traefik's static config, so a new address
		// means one restart. Done through the proxy service so the write and
		// the restart cannot be separated again.
		if err := s.px.SetResourceACME(ctx, r.ID, r.ACMEEmail); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// SetACME changes the Let's Encrypt account a host's certificates are issued
// on. Nothing else about a resource is editable: the host *is* its identity,
// and moving it would orphan every generated name nested under it.
func (s *DomainResourceService) SetACME(ctx context.Context, id, email string) error {
	r, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.checkOwner(ctx, r.Level, r.OwnerID, ""); err != nil {
		return err
	}
	return s.px.SetResourceACME(ctx, r.ID, email)
}

// Delete removes a resource. Generated hostnames under it keep working until
// their tile redeploys.
func (s *DomainResourceService) Delete(ctx context.Context, id string) error {
	r, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.checkOwner(ctx, r.Level, r.OwnerID, ""); err != nil {
		return err
	}
	return s.store.DeleteDomainResource(ctx, r.ID)
}

// Get reads one resource by id, or reports it missing.
func (s *DomainResourceService) Get(ctx context.Context, id string) (*repo.DomainResource, error) {
	all, err := s.store.ListDomainResources(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}
	return nil, svcerr.ErrNotFound
}

// checkOwner refuses a write the owner's config file owns, and (when host is
// given) a hostname that starts with another organization's slug.
//
// Squat used to be checked on the org page only, which made it decorative:
// the same name could be claimed from the API, from a stack-level resource,
// or one tile down.
func (s *DomainResourceService) checkOwner(ctx context.Context, level, ownerID, host string) error {
	switch level {
	case "stack":
		st, err := s.store.GetStack(ctx, ownerID)
		if err != nil || st == nil {
			return svcerr.ErrNotFound
		}
		if st.ConfigManaged() {
			return ManagedConflict(st)
		}
		if host != "" {
			return squat(ctx, s.store, host, st.OrgID)
		}
	case "org":
		o, err := s.store.GetOrg(ctx, ownerID)
		if err != nil || o == nil {
			return svcerr.ErrNotFound
		}
		if o.ConfigManaged() {
			return svcerr.Conflictf("organization is managed by %s; declare domains: in the org config file", o.ConfigRepo)
		}
		if host != "" {
			return squat(ctx, s.store, host, o.ID)
		}
	default: // instance: no config file behind it, and no org of its own
		if host != "" {
			// "" as the owning org means every org's slug is someone else's,
			// which is right: an instance-level host must not start with any
			// organization's slug.
			return squat(ctx, s.store, host, "")
		}
	}
	return nil
}

func squat(ctx context.Context, store repo.Store, host, ownOrgID string) error {
	if err := CheckOrgSquat(ctx, store, host, ownOrgID); err != nil {
		return svcerr.Conflict{Msg: err.Error()}
	}
	return nil
}

// canonLevel normalises the level and fills the instance owner. "local" is
// the single node a non-clustered install has, and three writers each picked
// their own value for it.
func canonLevel(level, ownerID string) (string, string) {
	switch level {
	case "org", "stack":
		return level, ownerID
	default:
		if ownerID == "" {
			ownerID = "local"
		}
		return "instance", ownerID
	}
}

// ListAll is every domain resource on the server: the hostnames stackr may
// generate names under. Filtering them to what a caller may see is the
// surface's job — the API's list does it per row against the caller's orgs —
// because "may see" is not a property of the row.
func (s *DomainResourceService) ListAll(ctx context.Context) ([]repo.DomainResource, error) {
	return s.store.ListDomainResources(ctx)
}

// Save writes a domain-resource row directly. Create is the one with the
// rules — the ownership check, the host validation; this is the raw write for
// the setup wizard, which creates the install's first hostname before there
// is an owner to check against.
func (s *DomainResourceService) Save(ctx context.Context, r *repo.DomainResource) error {
	return s.store.CreateDomainResource(ctx, r)
}

// The two domain rules below sit here, next to CheckOrgSquat, because they
// have the same shape: one rule about a hostname, needed by every surface that
// accepts one. DomainService applies them per request; config-as-code applies
// them at plan time, over a whole file, where a store read per domain is the
// wrong cost — so the rule is a pure function and the caller supplies the
// context. Two copies of a rule is how the config path came to write a route
// the panel would refuse.

// DomainPort resolves the container port a route proxies to: the domain's own
// port, else the tile's. Zero is only allowed for a redirect, which never
// proxies — and even then the port becomes 80, because 0 renders as
// http://alias:0, which is not a URL anyone meant.
func DomainPort(specPort, tilePort int, redirectTo string) (int, error) {
	port := specPort
	if port == 0 {
		port = tilePort
	}
	if port != 0 {
		return port, nil
	}
	if redirectTo == "" {
		return 0, invalid("container_port", "container port required (set it on the app or the domain)")
	}
	return 80, nil
}

// CheckWildcardHTTPS rejects a wildcard hostname served over TLS when no DNS
// provider is configured. A wildcard certificate can only be issued over
// DNS-01, so without one the certificate never issues and the refusal arrives
// as a hostname that does not answer.
func CheckWildcardHTTPS(host string, https, dnsConfigured bool) error {
	if !https || dnsConfigured || !strings.HasPrefix(host, "*.") {
		return nil
	}
	return invalid("host", "wildcard HTTPS needs a DNS provider; configure it in Settings")
}
